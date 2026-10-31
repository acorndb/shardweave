package backend

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/acorndb/prism/pkg/constant"
)

// Slice is one logical topology: master pool + optional slave pools (design §5.5).
// Pool leaves stay mode-agnostic; Slice only chooses which pool to Acquire.
type Slice struct {
	Name        string
	Master      *Pool
	Slaves      []*Pool
	LoadBalance string

	rr atomic.Uint64
}

// Acquire returns a backend and the pool that owns it (for Release/Discard).
// RoleSlave falls back to master when no slaves or all slave acquires fail.
func (s *Slice) Acquire(ctx context.Context, role constant.Role) (Conn, *Pool, error) {
	if s == nil || s.Master == nil {
		return nil, nil, fmt.Errorf("backend: slice has no master pool")
	}
	if role == constant.RoleMaster || len(s.Slaves) == 0 {
		b, err := s.Master.Acquire(ctx)
		return b, s.Master, err
	}

	order := s.slaveOrder()
	var lastErr error
	for _, p := range order {
		b, err := p.Acquire(ctx)
		if err == nil {
			return b, p, nil
		}
		lastErr = err
	}
	// fallback master (kingshard / Gaea / dbpack)
	b, err := s.Master.Acquire(ctx)
	if err != nil {
		if lastErr != nil {
			return nil, nil, fmt.Errorf("backend: slave acquire failed (%v); master: %w", lastErr, err)
		}
		return nil, nil, err
	}
	return b, s.Master, nil
}

func (s *Slice) slaveOrder() []*Pool {
	n := len(s.Slaves)
	if n == 0 {
		return nil
	}
	if s.LoadBalance == constant.LoadBalanceRandom {
		start := int(s.rr.Add(1) % uint64(n))
		out := make([]*Pool, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, s.Slaves[(start+i)%n])
		}
		return out
	}
	// default round_robin: rotate start each call
	start := int(s.rr.Add(1)-1) % n
	out := make([]*Pool, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s.Slaves[(start+i)%n])
	}
	return out
}

// Release returns b to its owning pool.
func (s *Slice) Release(p *Pool, b Conn) error {
	if p == nil {
		return nil
	}
	return p.Release(b)
}

// Discard drops a sticky backend without reset-to-idle.
func (s *Slice) Discard(p *Pool, b Conn) error {
	if p == nil {
		return nil
	}
	return p.Discard(b)
}

// Close closes master and slave pools.
func (s *Slice) Close() error {
	if s == nil {
		return nil
	}
	var first error
	if s.Master != nil {
		if err := s.Master.Close(); err != nil && first == nil {
			first = err
		}
	}
	for _, p := range s.Slaves {
		if p == nil {
			continue
		}
		if err := p.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
