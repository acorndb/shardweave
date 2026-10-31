// Package execute implements the single data-plane execution pipeline.
package execute

import (
	"context"
	"fmt"

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
	status, serverErr, err := raw.ForwardTo(ctx, rt.Front)
	if err == nil && !serverErr {
		if !req.Prepare && !req.NoReply {
			rt.State.SetInTx(status&mysql.ServerStatusInTrans != 0)
		}
		applyEffect(rt.State, stmt.Effect)
	}
	return err
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
