// Package executor implements mode-specific backend strategies (design: executor-three-mode).
package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/acorndb/prism/pkg/backend_server"
	"github.com/acorndb/prism/pkg/constant"
	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/mysql/packet"
	"github.com/acorndb/prism/pkg/router"
	"github.com/acorndb/prism/pkg/session"
)

// ErrShardingNotImplemented is returned by ShardingExecutor.Handle.
var ErrShardingNotImplemented = errors.New("executor: sharding not implemented")

// Conn is per frontend-connection mutable handles (not pooled).
type Conn struct {
	Front      *packet.Conn
	State      *session.State
	Sticky     backend_server.Backend
	StickyPool *backend_server.Pool
}

// Request is one frontend command for Handle.
type Request struct {
	Cmd     byte
	Payload []byte
	SQL     string
}

// Executor is the process-shared strategy (dbpack-style three modes).
type Executor interface {
	Mode() string
	Handle(ctx context.Context, c *Conn, req Request) error
	ReleaseSticky(c *Conn, discard bool) error
	Close() error
}

// New builds an Executor for backend_server.mode.
func New(mode string, g *router.Group) (Executor, error) {
	if mode == "" {
		mode = constant.ModeSingle
	}
	switch mode {
	case constant.ModeSingle:
		if g == nil || g.Master == nil {
			return nil, fmt.Errorf("executor: single requires master pool")
		}
		return &passthrough{
			mode:  constant.ModeSingle,
			group: g,
			pick: func(*session.State, byte, string) router.TargetRole {
				return router.RoleMaster
			},
		}, nil
	case constant.ModeRWSplit:
		if g == nil || g.Master == nil {
			return nil, fmt.Errorf("executor: rwsplit requires master pool")
		}
		return &passthrough{
			mode:  constant.ModeRWSplit,
			group: g,
			pick:  router.Pick,
		}, nil
	case constant.ModeSharding:
		return &shardingExecutor{}, nil
	default:
		return nil, fmt.Errorf("executor: unknown mode %q", mode)
	}
}

type passthrough struct {
	mode  string
	group *router.Group
	pick  func(*session.State, byte, string) router.TargetRole
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

func (p *passthrough) Handle(ctx context.Context, c *Conn, req Request) error {
	if c == nil || c.State == nil {
		return fmt.Errorf("executor: nil conn state")
	}
	switch req.Cmd {
	case mysql.ComStmtClose:
		return p.withBackend(ctx, c, req, func(b backend_server.Backend) error {
			return writeBackend(b, req.Payload)
		})
	case mysql.ComStmtPrepare:
		return p.withBackend(ctx, c, req, func(b backend_server.Backend) error {
			return execCommand(c.Front, b, c.State, req.Payload, true)
		})
	default:
		return p.withBackend(ctx, c, req, func(b backend_server.Backend) error {
			return execCommand(c.Front, b, c.State, req.Payload, false)
		})
	}
}

func (p *passthrough) withBackend(
	ctx context.Context,
	c *Conn,
	req Request,
	fn func(backend_server.Backend) error,
) error {
	var b backend_server.Backend
	var pool *backend_server.Pool
	var err error

	if c.Sticky != nil {
		b = c.Sticky
		pool = c.StickyPool
	} else {
		role := p.pick(c.State, req.Cmd, req.SQL)
		b, pool, err = p.group.Acquire(ctx, role)
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

func (s *shardingExecutor) Handle(context.Context, *Conn, Request) error {
	return ErrShardingNotImplemented
}

func (s *shardingExecutor) ReleaseSticky(*Conn, bool) error { return nil }

func (s *shardingExecutor) Close() error { return nil }
