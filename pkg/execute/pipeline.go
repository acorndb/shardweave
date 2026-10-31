// Package execute implements the single data-plane execution pipeline.
package execute

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/acorndb/shardweave/pkg/backend"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/route"
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

	if stmt.Kind == sql.KindExplainRoute {
		rt.State.SetRowCount(-1)
		return p.explainRoute(ctx, rt, stmt)
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
	success, err = p.executePlan(ctx, rt, stmt, pl)
	return err
}

// executePlan dispatches one plan. N=1 keeps the raw fast path; N>1 is the
// sharding seam and currently returns a stable not-implemented error.
func (p *Pipeline) executePlan(ctx context.Context, rt *Runtime, stmt *sql.Statement, pl *plan.Plan) (bool, error) {
	switch len(pl.Targets) {
	case 1:
		return p.runOne(ctx, rt, stmt, pl.Targets[0])
	default:
		return p.dispatch(ctx, rt, stmt, pl)
	}
}

func (p *Pipeline) dispatch(context.Context, *Runtime, *sql.Statement, *plan.Plan) (bool, error) {
	return false, fmt.Errorf("execute: multi-target not implemented")
}

// runOne executes one target, acquires/reuses its lease and forwards the raw
// response. It returns whether session state was successfully updated.
func (p *Pipeline) runOne(ctx context.Context, rt *Runtime, stmt *sql.Statement, target route.Target) (bool, error) {
	if rt.leases == nil {
		rt.leases = make(map[string]*lease)
	}
	l := rt.leases[target.Slice]
	acquired := false
	if l == nil {
		slice, err := p.cluster.Get(target.Slice)
		if err != nil {
			return false, err
		}
		conn, pool, err := slice.Acquire(ctx, target.Role)
		if err != nil {
			return false, err
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
		return false, err
	}

	req := backend.ExecRequest{
		Payload: payloadFor(stmt, target),
		Prepare: stmt.Cmd == mysql.ComStmtPrepare,
		NoReply: stmt.Cmd == mysql.ComStmtClose,
	}
	stream, err := l.conn.Exec(ctx, req)
	if err != nil {
		return false, err
	}
	defer stream.Close()

	raw, ok := stream.(rawForwarder)
	if !ok {
		return false, fmt.Errorf("execute: stream %T not forwardable", stream)
	}
	info, err := raw.ForwardTo(ctx, rt.Front)
	if err != nil {
		return false, err
	}
	if info.ServerErr {
		return false, nil // ERR already forwarded; rowCount is marked -1
	}
	if !req.Prepare && !req.NoReply {
		rt.State.SetInTx(info.Status&mysql.ServerStatusInTrans != 0)
	}
	applyEffect(rt.State, stmt.Effect)
	applyResponseState(rt.State, stmt, info)
	return true, nil
}

// payloadFor uses the per-target rewritten SQL when present. Only COM_QUERY
// has a text SQL representation; prepared commands keep their original bytes.
func payloadFor(stmt *sql.Statement, target route.Target) []byte {
	if stmt.Cmd != mysql.ComQuery || target.SQL == "" {
		return stmt.Payload
	}
	payload := make([]byte, 1+len(target.SQL))
	payload[0] = mysql.ComQuery
	copy(payload[1:], target.SQL)
	return payload
}

// explainRoute builds a plan without executing it and writes a synthetic
// result set describing targets or the rejection reason.
func (p *Pipeline) explainRoute(ctx context.Context, rt *Runtime, stmt *sql.Statement) error {
	var (
		pl       *plan.Plan
		buildErr error
	)
	if stmt.Err != nil {
		buildErr = stmt.Err
	} else {
		pl, buildErr = p.builder.Build(ctx, rt.State, stmt)
	}
	cols, rows := explainRows(stmt, pl, buildErr)
	return writeTextResult(rt.Front, cols, rows)
}

func explainRows(stmt *sql.Statement, pl *plan.Plan, buildErr error) ([]string, [][]string) {
	cols := []string{"kind", "slice", "role", "db", "sql", "reason"}
	if buildErr != nil {
		var rej *plan.RejectError
		if errors.As(buildErr, &rej) {
			return cols, [][]string{{"Reject", "", "", "", "", rej.Message}}
		}
		return cols, [][]string{{"Error", "", "", "", "", buildErr.Error()}}
	}
	if pl == nil {
		return cols, [][]string{{"Error", "", "", "", "", "nil plan"}}
	}
	if pl.Kind == plan.Reject {
		reason := ""
		if pl.Reject != nil {
			reason = pl.Reject.Message
		}
		return cols, [][]string{{"Reject", "", "", "", "", reason}}
	}
	rows := make([][]string, 0, len(pl.Targets))
	for _, t := range pl.Targets {
		sqlText := t.SQL
		if sqlText == "" {
			sqlText = stmt.Raw
		}
		rows = append(rows, []string{
			pl.Kind.String(), t.Slice, string(t.Role), t.DB, sqlText, "",
		})
	}
	return cols, rows
}

func writeTextResult(front *packet.Conn, cols []string, rows [][]string) error {
	if front == nil {
		return fmt.Errorf("execute: nil frontend for explain")
	}
	if err := writeFrontPayload(front, mysql.EncodeColumnCount(uint64(len(cols)))); err != nil {
		return err
	}
	for _, name := range cols {
		def := mysql.EncodeColumnDefinition(mysql.Column{
			Name: name, Charset: 0x2d, ColumnLength: 1024, Type: 0xfd, // VAR_STRING
		})
		if err := writeFrontPayload(front, def); err != nil {
			return err
		}
	}
	for _, row := range rows {
		cells := make([][]byte, len(row))
		for i, v := range row {
			cells[i] = []byte(v)
		}
		if err := writeFrontPayload(front, mysql.EncodeTextRow(cells)); err != nil {
			return err
		}
	}
	// The frontend handshake advertises CLIENT_DEPRECATE_EOF; keep this in sync
	// if capability negotiation becomes per-session.
	return writeFrontPayload(front, mysql.EncodeEOF(true, mysql.ServerStatusAutocommit, 0))
}

func writeFrontPayload(front *packet.Conn, payload []byte) error {
	buf := make([]byte, 4+len(payload))
	copy(buf[4:], payload)
	return front.WritePacket(buf)
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
