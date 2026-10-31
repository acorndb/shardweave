// Package execute implements the single data-plane execution pipeline.
package execute

import (
	"context"
	"fmt"
	"strconv"

	"github.com/acorndb/shardweave/pkg/backend"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
)

// ErrPoolExhausted is returned when the backend pool is at capacity and
// acquire_timeout is zero (fail-fast).
var ErrPoolExhausted = backend.ErrPoolExhausted

// Runtime is per frontend-connection mutable state.
type Runtime struct {
	Front  *packet.Conn
	State  *session.State
	leases map[string]*lease // slice name -> lease
}

type lease struct {
	slice *backend.Slice
	conn  backend.Conn
	pool  *backend.Pool
}

// Executor is the process-shared execution strategy.
type Executor interface {
	Handle(ctx context.Context, rt *Runtime, stmt *sql.Statement) error
	Release(rt *Runtime, discard bool) error
	Close() error
}

// Pipeline is the single execution implementation shared by all modes.
type Pipeline struct {
	cluster *backend.Cluster
	builder plan.Builder
}

// New builds a Pipeline for a runtime cluster and a mode-specific planner.
func New(c *backend.Cluster, builder plan.Builder) (*Pipeline, error) {
	if c == nil || len(c.Names()) == 0 {
		return nil, fmt.Errorf("execute: requires cluster")
	}
	if builder.Router == nil {
		return nil, fmt.Errorf("execute: requires router")
	}
	return &Pipeline{cluster: c, builder: builder}, nil
}

// Close closes the underlying cluster pools.
func (p *Pipeline) Close() error {
	if p == nil || p.cluster == nil {
		return nil
	}
	return p.cluster.Close()
}

func (p *Pipeline) releaseOne(rt *Runtime, sliceName string, discard bool) error {
	if rt == nil || rt.leases == nil {
		return nil
	}
	l := rt.leases[sliceName]
	if l == nil {
		return nil
	}
	delete(rt.leases, sliceName)
	if discard {
		return l.slice.Discard(l.pool, l.conn)
	}
	return l.slice.Release(l.pool, l.conn)
}

// Release returns or discards every lease held by the session.
func (p *Pipeline) Release(rt *Runtime, discard bool) error {
	if p == nil || p.cluster == nil || rt == nil || len(rt.leases) == 0 {
		return nil
	}
	var first error
	for name := range rt.leases {
		if err := p.releaseOne(rt, name, discard); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Handle builds a Plan, acquires/reuses a lease, executes and forwards the result.
func (p *Pipeline) Handle(ctx context.Context, rt *Runtime, stmt *sql.Statement) error {
	if p == nil || p.cluster == nil {
		return fmt.Errorf("execute: nil pipeline")
	}
	if rt == nil || rt.State == nil {
		return fmt.Errorf("execute: nil runtime state")
	}
	if stmt == nil {
		return fmt.Errorf("execute: nil statement")
	}

	rewriteSessionFunc(rt.State, stmt)
	success := false
	defer func() {
		if !success {
			markRowCountError(rt.State, stmt)
		}
	}()

	pl, err := p.builder.Build(ctx, rt.State, stmt)
	if err != nil {
		return err
	}
	if pl.Kind == plan.Reject {
		return pl.Reject
	}
	if len(pl.Targets) != 1 {
		return fmt.Errorf("execute: expected 1 target, got %d", len(pl.Targets))
	}
	target := pl.Targets[0]

	if rt.leases == nil {
		rt.leases = make(map[string]*lease)
	}
	l := rt.leases[target.Slice]
	acquired := false
	if l == nil {
		slice, err := p.cluster.Get(target.Slice)
		if err != nil {
			return err
		}
		conn, pool, err := slice.Acquire(ctx, target.Role)
		if err != nil {
			return err
		}
		l = &lease{slice: slice, conn: conn, pool: pool}
		rt.leases[target.Slice] = l
		acquired = true
	}
	// Evaluate stickiness after execution: BEGIN sets InTx from the backend status.
	defer func() {
		if !rt.State.NeedsSticky() {
			_ = p.Release(rt, false)
		}
	}()

	db := target.DB
	if db == "" {
		db = rt.State.Database()
	}
	if err := l.conn.Align(ctx, backend.AlignSettings{Database: db, Charset: rt.State.Charset()}); err != nil {
		if acquired {
			_ = p.releaseOne(rt, target.Slice, false)
		}
		return err
	}

	req := backend.ExecRequest{
		Payload: stmt.Payload,
		Prepare: stmt.Cmd == mysql.ComStmtPrepare,
		NoReply: stmt.Cmd == mysql.ComStmtClose,
	}
	stream, err := l.conn.Exec(ctx, req)
	if err != nil {
		return err
	}
	defer stream.Close()

	raw, ok := stream.(rawForwarder)
	if !ok {
		return fmt.Errorf("execute: stream %T not forwardable", stream)
	}
	info, err := raw.ForwardTo(ctx, rt.Front)
	if err == nil && !info.ServerErr {
		if !req.Prepare && !req.NoReply {
			rt.State.SetInTx(info.Status&mysql.ServerStatusInTrans != 0)
		}
		applyEffect(rt.State, stmt.Effect)
		applyResponseState(rt.State, stmt, info)
		success = true
	}
	return err
}

// rewriteSessionFunc answers SELECT LAST_INSERT_ID()/ROW_COUNT() from session
// state. The rewritten literal is sent to a backend so the proxy does not need
// to synthesize a result set; the value is independent of the backend data.
func rewriteSessionFunc(st *session.State, stmt *sql.Statement) {
	if stmt.Cmd != mysql.ComQuery {
		return
	}
	fn, ok := sql.ParseSessionFunc(stmt.Raw)
	if !ok {
		return
	}
	var value string
	switch fn.Kind {
	case sql.SessionFuncLastInsertID:
		value = strconv.FormatUint(st.LastInsertID(), 10)
	case sql.SessionFuncRowCount:
		value = strconv.FormatInt(st.RowCount(), 10)
	}
	rewritten := fn.Rewrite(value)
	payload := make([]byte, 1+len(rewritten))
	payload[0] = mysql.ComQuery
	copy(payload[1:], rewritten)
	stmt.Payload = payload
	stmt.Raw = rewritten
}

// applyResponseState mirrors MySQL's connection-scoped function semantics:
// LAST_INSERT_ID persists across statements (only a non-zero OK value updates
// it); ROW_COUNT is -1 for result sets/errors, affected_rows for DML, and 0 for
// other OK results.
func applyResponseState(st *session.State, stmt *sql.Statement, info mysql.ResponseInfo) {
	if info.LastInsertID != 0 {
		st.SetLastInsertID(info.LastInsertID)
	}
	switch stmt.Cmd {
	case mysql.ComQuery, mysql.ComStmtExecute:
	default:
		return
	}
	if info.ResultSet {
		st.SetRowCount(-1)
		return
	}
	if stmt.Cmd == mysql.ComStmtExecute || sql.IsDML(stmt.Raw) {
		st.SetRowCount(int64(info.AffectedRows))
		return
	}
	st.SetRowCount(0)
}

func markRowCountError(st *session.State, stmt *sql.Statement) {
	if stmt == nil {
		return
	}
	switch stmt.Cmd {
	case mysql.ComQuery, mysql.ComStmtExecute:
		st.SetRowCount(-1)
	}
}

// applyEffect applies a statement's session effect only after a successful
// backend response. Autocommit must be applied before the deferred sticky check.
func applyEffect(st *session.State, e sql.Effect) {
	if e.UseDB != "" {
		st.SetDatabase(e.UseDB)
	}
	if e.Charset != "" {
		st.SetCharset(e.Charset)
	}
	if e.Autocommit != nil {
		st.SetAutocommit(*e.Autocommit)
	}
}
