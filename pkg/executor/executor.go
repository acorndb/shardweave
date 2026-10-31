// Package executor implements mode-specific backend strategies (design: executor-three-mode).
package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/acorndb/prism/pkg/backend"
	"github.com/acorndb/prism/pkg/constant"
	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/mysql/packet"
	"github.com/acorndb/prism/pkg/plan"
	"github.com/acorndb/prism/pkg/route/source"
	"github.com/acorndb/prism/pkg/session"
	"github.com/acorndb/prism/pkg/sql"
)

// ErrShardingNotImplemented is returned by ShardingExecutor.Handle.
var ErrShardingNotImplemented = errors.New("executor: sharding not implemented")

// Conn is per frontend-connection mutable handles (not pooled).
type Conn struct {
	Front      *packet.Conn
	State      *session.State
	Sticky     backend.Conn
	StickyPool *backend.Pool
}

// Executor is the process-shared strategy (dbpack-style three modes).
type Executor interface {
	Mode() string
	Handle(ctx context.Context, c *Conn, stmt *sql.Statement) error
	ReleaseSticky(c *Conn, discard bool) error
	Close() error
}

// New builds an Executor for backend_server.mode.
func New(mode string, g *backend.Slice) (Executor, error) {
	if mode == "" {
		mode = constant.ModeSingle
	}
	switch mode {
	case constant.ModeSingle:
		if g == nil || g.Master == nil {
			return nil, fmt.Errorf("executor: single requires master pool")
		}
		return &passthrough{
			mode:    constant.ModeSingle,
			group:   g,
			builder: plan.Builder{Router: source.AllMaster{}},
		}, nil
	case constant.ModeRWSplit:
		if g == nil || g.Master == nil {
			return nil, fmt.Errorf("executor: rwsplit requires master pool")
		}
		return &passthrough{
			mode:    constant.ModeRWSplit,
			group:   g,
			builder: plan.Builder{Router: source.Pick{}},
		}, nil
	case constant.ModeSharding:
		return &shardingExecutor{}, nil
	default:
		return nil, fmt.Errorf("executor: unknown mode %q", mode)
	}
}

type passthrough struct {
	mode    string
	group   *backend.Slice
	builder plan.Builder
}

func (p *passthrough) Mode() string { return p.mode }

func (p *passthrough) Close() error {
	if p == nil || p.group == nil {
		return nil
	}
	return p.group.Close()
}

func (p *passthrough) ReleaseSticky(c *Conn, discard bool) error {
	if c == nil || c.Sticky == nil {
		return nil
	}
	b, pool := c.Sticky, c.StickyPool
	c.Sticky = nil
	c.StickyPool = nil
	if discard {
		return p.group.Discard(pool, b)
	}
	return p.group.Release(pool, b)
}

func (p *passthrough) Handle(ctx context.Context, c *Conn, stmt *sql.Statement) error {
	if c == nil || c.State == nil {
		return fmt.Errorf("executor: nil conn state")
	}
	if stmt == nil {
		return fmt.Errorf("executor: nil statement")
	}
	switch stmt.Cmd {
	case mysql.ComStmtClose:
		return p.withBackend(ctx, c, stmt, func(b backend.Conn) error {
			return writeBackend(b, stmt.Payload)
		})
	case mysql.ComStmtPrepare:
		return p.withBackend(ctx, c, stmt, func(b backend.Conn) error {
			return execCommand(c.Front, b, c.State, stmt.Payload, true)
		})
	default:
		return p.withBackend(ctx, c, stmt, func(b backend.Conn) error {
			return execCommand(c.Front, b, c.State, stmt.Payload, false)
		})
	}
}

func (p *passthrough) withBackend(
	ctx context.Context,
	c *Conn,
	stmt *sql.Statement,
	fn func(backend.Conn) error,
) error {
	pl, err := p.builder.Build(ctx, c.State, stmt)
	if err != nil {
		return err
	}
	if pl.Kind == plan.Reject {
		return pl.Reject
	}

	var b backend.Conn
	var pool *backend.Pool
	if c.Sticky != nil {
		b = c.Sticky
		pool = c.StickyPool
	} else {
		b, pool, err = p.group.Acquire(ctx, pl.Targets[0].Role)
		if err != nil {
			return err
		}
	}

	if err := alignBackend(ctx, b, c.State); err != nil {
		if c.Sticky == nil {
			_ = p.group.Release(pool, b)
		}
		return err
	}

	ferr := fn(b)

	if c.State.NeedsSticky() {
		c.Sticky = b
		c.StickyPool = pool
		return ferr
	}
	c.Sticky = nil
	c.StickyPool = nil
	_ = p.group.Release(pool, b)
	return ferr
}

type shardingExecutor struct{}

func (s *shardingExecutor) Mode() string { return constant.ModeSharding }

func (s *shardingExecutor) Handle(context.Context, *Conn, *sql.Statement) error {
	return ErrShardingNotImplemented
}

func (s *shardingExecutor) ReleaseSticky(*Conn, bool) error { return nil }

func (s *shardingExecutor) Close() error { return nil }
