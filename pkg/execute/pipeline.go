// Package execute implements the single data-plane execution pipeline.
package execute

import (
	"context"
	"fmt"

	"github.com/acorndb/prism/pkg/backend"
	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/mysql/packet"
	"github.com/acorndb/prism/pkg/plan"
	"github.com/acorndb/prism/pkg/session"
	"github.com/acorndb/prism/pkg/sql"
)

// Runtime is per frontend-connection mutable state.
type Runtime struct {
	Front *packet.Conn
	State *session.State
	lease *lease
}

type lease struct {
	conn backend.Conn
	pool *backend.Pool
}

// Executor is the process-shared execution strategy.
type Executor interface {
	Handle(ctx context.Context, rt *Runtime, stmt *sql.Statement) error
	Release(rt *Runtime, discard bool) error
	Close() error
}

// Pipeline is the single execution implementation shared by all modes.
type Pipeline struct {
	group   *backend.Slice
	builder plan.Builder
}

// New builds a Pipeline for one backend slice and a mode-specific planner.
func New(g *backend.Slice, builder plan.Builder) (*Pipeline, error) {
	if g == nil || g.Master == nil {
		return nil, fmt.Errorf("execute: requires master pool")
	}
	if builder.Router == nil {
		return nil, fmt.Errorf("execute: requires router")
	}
	return &Pipeline{group: g, builder: builder}, nil
}

// Close closes the underlying slice pools.
func (p *Pipeline) Close() error {
	if p == nil || p.group == nil {
		return nil
	}
	return p.group.Close()
}

// Release returns the current lease. discard drops it without reset-to-idle.
func (p *Pipeline) Release(rt *Runtime, discard bool) error {
	if p == nil || p.group == nil || rt == nil || rt.lease == nil {
		return nil
	}
	l := rt.lease
	rt.lease = nil
	if discard {
		return p.group.Discard(l.pool, l.conn)
	}
	return p.group.Release(l.pool, l.conn)
}

// Handle builds a Plan, acquires/reuses a lease, executes and forwards the result.
func (p *Pipeline) Handle(ctx context.Context, rt *Runtime, stmt *sql.Statement) error {
	if p == nil || p.group == nil {
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

	acquired := false
	if rt.lease == nil {
		conn, pool, err := p.group.Acquire(ctx, target.Role)
		if err != nil {
			return err
		}
		rt.lease = &lease{conn: conn, pool: pool}
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
	if err := rt.lease.conn.Align(ctx, backend.AlignSettings{Database: db, Charset: rt.State.Charset()}); err != nil {
		if acquired {
			_ = p.Release(rt, false)
		}
		return err
	}

	req := backend.ExecRequest{
		Payload: stmt.Payload,
		Prepare: stmt.Cmd == mysql.ComStmtPrepare,
		NoReply: stmt.Cmd == mysql.ComStmtClose,
	}
	stream, err := rt.lease.conn.Exec(ctx, req)
	if err != nil {
		return err
	}
	defer stream.Close()

	raw, ok := stream.(rawForwarder)
	if !ok {
		return fmt.Errorf("execute: stream %T not forwardable", stream)
	}
	status, err := raw.ForwardTo(ctx, rt.Front)
	if err == nil && !req.Prepare && !req.NoReply {
		rt.State.SetInTx(status&mysql.ServerStatusInTrans != 0)
	}
	return err
}
