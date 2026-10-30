package router

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/acorndb/prism/pkg/backend_server"
)

// Load-balance names (backend_server.load_balance).
const (
	LBRoundRobin = "round_robin"
	LBRandom     = "random"
)

// Group is one logical topology: master pool + optional slave pools (design §5.5).
// Pool leaves stay mode-agnostic; Group only chooses which pool to Acquire.
type Group struct {
	Master      *backend_server.Pool
	Slaves      []*backend_server.Pool
	LoadBalance string

	rr atomic.Uint64
}

// Acquire returns a backend and the pool that owns it (for Release/Discard).
// RoleSlave falls back to master when no slaves or all slave acquires fail.
func (g *Group) Acquire(ctx context.Context, role TargetRole) (backend_server.Backend, *backend_server.Pool, error) {
	if g == nil || g.Master == nil {
		return nil, nil, fmt.Errorf("router: group has no master pool")
	}
	if role == RoleMaster || len(g.Slaves) == 0 {
		b, err := g.Master.Acquire(ctx)
		return b, g.Master, err
	}

	order := g.slaveOrder()
	var lastErr error
	for _, p := range order {
		b, err := p.Acquire(ctx)
		if err == nil {
			return b, p, nil
		}
		lastErr = err
	}
	// fallback master (kingshard / Gaea / dbpack)
	b, err := g.Master.Acquire(ctx)
	if err != nil {
		if lastErr != nil {
			return nil, nil, fmt.Errorf("router: slave acquire failed (%v); master: %w", lastErr, err)
		}
		return nil, nil, err
	}
	return b, g.Master, nil
}

func (g *Group) slaveOrder() []*backend_server.Pool {
	n := len(g.Slaves)
	if n == 0 {
		return nil
	}
	if g.LoadBalance == LBRandom {
		start := int(g.rr.Add(1) % uint64(n))
		out := make([]*backend_server.Pool, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, g.Slaves[(start+i)%n])
		}
		return out
	}
	// default round_robin: rotate start each call
	start := int(g.rr.Add(1)-1) % n
	out := make([]*backend_server.Pool, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, g.Slaves[(start+i)%n])
	}
	return out
}

// Release returns b to its owning pool.
func (g *Group) Release(p *backend_server.Pool, b backend_server.Backend) error {
	if p == nil {
		return nil
	}
	return p.Release(b)
}

// Discard drops a sticky backend without reset-to-idle.
func (g *Group) Discard(p *backend_server.Pool, b backend_server.Backend) error {
	if p == nil {
		return nil
	}
	return p.Discard(b)
}

// Close closes master and slave pools.
func (g *Group) Close() error {
	if g == nil {
		return nil
	}
	var first error
	if g.Master != nil {
		if err := g.Master.Close(); err != nil && first == nil {
			first = err
		}
	}
	for _, s := range g.Slaves {
		if s == nil {
			continue
		}
		if err := s.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
