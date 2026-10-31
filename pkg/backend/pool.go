package backend

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// OpenFunc creates a new backend connection.
type OpenFunc func(ctx context.Context) (Conn, error)

// PoolConfig controls pool sizing and acquire timeout.
type PoolConfig struct {
	Capacity    int
	MaxCapacity int
	IdleTimeout time.Duration
	GetTimeout  time.Duration
}

// Pool is a simple bounded pool (gaea/vitess-style semantics, single backend target).
type Pool struct {
	cfg  PoolConfig
	open OpenFunc

	mu     sync.Mutex
	idle   []Conn
	active int
	closed bool
}

// NewPool creates a Pool. Capacity is the initial idle target (lazy — no pre-create);
// MaxCapacity caps total live connections.
func NewPool(cfg PoolConfig, open OpenFunc) *Pool {
	if cfg.MaxCapacity <= 0 {
		cfg.MaxCapacity = 1
	}
	if cfg.Capacity < 0 {
		cfg.Capacity = 0
	}
	if cfg.Capacity > cfg.MaxCapacity {
		cfg.Capacity = cfg.MaxCapacity
	}
	if cfg.GetTimeout <= 0 {
		cfg.GetTimeout = 2 * time.Second
	}
	return &Pool{cfg: cfg, open: open}
}

// Acquire returns an idle backend or opens a new one up to MaxCapacity.
func (p *Pool) Acquire(ctx context.Context) (Conn, error) {
	deadline := time.Now().Add(p.cfg.GetTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}

	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("pool: closed")
		}
		if len(p.idle) > 0 {
			b := p.idle[len(p.idle)-1]
			p.idle = p.idle[:len(p.idle)-1]
			p.active++
			p.mu.Unlock()
			return b, nil
		}
		if p.active < p.cfg.MaxCapacity {
			p.active++
			open := p.open
			p.mu.Unlock()
			b, err := open(ctx)
			if err != nil {
				p.mu.Lock()
				p.active--
				p.mu.Unlock()
				return nil, err
			}
			return b, nil
		}
		p.mu.Unlock()

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("pool: get timeout after %s", p.cfg.GetTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Release resets and returns b to the idle list, or closes it on reset failure.
func (p *Pool) Release(b Conn) error {
	if b == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.GetTimeout)
	defer cancel()
	if err := b.Reset(ctx); err != nil {
		_ = b.Close()
		p.mu.Lock()
		p.active--
		p.mu.Unlock()
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.active--
	if p.closed {
		_ = b.Close()
		return nil
	}
	p.idle = append(p.idle, b)
	return nil
}

// Discard closes b and drops it from the in-use count without returning it to the idle list.
// Used when a sticky/reserved frontend session disconnects (Vitess-style terminate).
func (p *Pool) Discard(b Conn) error {
	if b == nil {
		return nil
	}
	err := b.Close()
	p.mu.Lock()
	p.active--
	p.mu.Unlock()
	return err
}

// Close closes idle backends; in-use backends are closed when released.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	var first error
	for _, b := range p.idle {
		if err := b.Close(); err != nil && first == nil {
			first = err
		}
	}
	p.idle = nil
	return first
}
