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

// PoolConfig controls pool sizing, health checks and recycling.
type PoolConfig struct {
	Capacity            int           // initial idle target (best-effort async warmup)
	MaxCapacity         int           // max live connections (borrowed + idle)
	MaxIdle             int           // max idle connections (default MaxCapacity)
	IdleTimeout         time.Duration // close idle connections after this duration (0 = disabled)
	MaxLifetime         time.Duration // close connections older than this (0 = unlimited)
	HealthCheckInterval time.Duration // ping idle conns older than this (0 = default 30s; <0 disables)
	GetTimeout          time.Duration // acquire / ping / reset timeout
}

type idleConn struct {
	conn  Conn
	since time.Time
}

// Pool is a simple bounded pool.
type Pool struct {
	cfg  PoolConfig
	open OpenFunc

	mu      sync.Mutex
	idle    []idleConn
	active  int // borrowed + opening
	created map[Conn]time.Time
	closed  bool

	stop     chan struct{}
	stopOnce sync.Once
}

// NewPool creates a Pool. Capacity is the initial idle target;
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
	if cfg.MaxIdle <= 0 {
		cfg.MaxIdle = cfg.MaxCapacity
	}
	if cfg.MaxIdle > cfg.MaxCapacity {
		cfg.MaxIdle = cfg.MaxCapacity
	}
	if cfg.Capacity > cfg.MaxIdle {
		cfg.Capacity = cfg.MaxIdle
	}
	if cfg.GetTimeout <= 0 {
		cfg.GetTimeout = 2 * time.Second
	}
	if cfg.HealthCheckInterval == 0 {
		cfg.HealthCheckInterval = 30 * time.Second
	}
	if cfg.HealthCheckInterval < 0 {
		cfg.HealthCheckInterval = 0
	}

	p := &Pool{
		cfg:     cfg,
		open:    open,
		created: make(map[Conn]time.Time),
		stop:    make(chan struct{}),
	}
	if cfg.Capacity > 0 {
		go p.warmup()
	}
	if cfg.IdleTimeout > 0 || cfg.MaxLifetime > 0 {
		go p.reaper()
	}
	return p
}

func (p *Pool) liveLocked() int {
	return p.active + len(p.idle)
}

// Stats is a point-in-time pool snapshot for diagnostics/metrics.
type Stats struct {
	Active int
	Idle   int
	Max    int
}

// Stats returns the current pool counters.
func (p *Pool) Stats() Stats {
	if p == nil {
		return Stats{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{Active: p.active, Idle: len(p.idle), Max: p.cfg.MaxCapacity}
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
			ic := p.idle[len(p.idle)-1]
			p.idle = p.idle[:len(p.idle)-1]
			p.active++
			created := p.created[ic.conn]
			p.mu.Unlock()

			if p.expired(created) {
				_ = ic.conn.Close()
				p.mu.Lock()
				p.active--
				delete(p.created, ic.conn)
				p.mu.Unlock()
				continue
			}
			if p.needPing(ic.since) {
				pingCtx, cancel := context.WithTimeout(ctx, p.cfg.GetTimeout)
				err := ic.conn.Ping(pingCtx)
				cancel()
				if err != nil {
					_ = ic.conn.Close()
					p.mu.Lock()
					p.active--
					delete(p.created, ic.conn)
					p.mu.Unlock()
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					continue
				}
			}
			return ic.conn, nil
		}
		if p.liveLocked() < p.cfg.MaxCapacity {
			p.active++
			open := p.open
			p.mu.Unlock()
			if open == nil {
				p.mu.Lock()
				p.active--
				p.mu.Unlock()
				return nil, errors.New("pool: nil open func")
			}
			b, err := open(ctx)
			if err != nil {
				p.mu.Lock()
				p.active--
				p.mu.Unlock()
				return nil, err
			}
			if b == nil {
				p.mu.Lock()
				p.active--
				p.mu.Unlock()
				return nil, errors.New("pool: open returned nil conn")
			}
			p.mu.Lock()
			if p.closed {
				if p.active > 0 {
					p.active--
				}
				p.mu.Unlock()
				_ = b.Close()
				return nil, errors.New("pool: closed")
			}
			p.created[b] = time.Now()
			p.mu.Unlock()
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

// Release resets and returns b to the idle list, or closes it when the pool is
// closed, b is too old, or MaxIdle is reached.
func (p *Pool) Release(b Conn) error {
	if b == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.GetTimeout)
	defer cancel()
	resetErr := b.Reset(ctx)

	p.mu.Lock()
	if p.active > 0 {
		p.active--
	}
	created, hasCreated := p.created[b]
	if !hasCreated {
		created = time.Now()
		p.created[b] = created
	}
	expired := p.cfg.MaxLifetime > 0 && time.Since(created) >= p.cfg.MaxLifetime
	full := len(p.idle) >= p.cfg.MaxIdle
	if resetErr != nil || p.closed || expired || full {
		delete(p.created, b)
		p.mu.Unlock()
		_ = b.Close()
		return resetErr
	}
	p.idle = append(p.idle, idleConn{conn: b, since: time.Now()})
	p.mu.Unlock()
	return nil
}

// Discard closes b and drops it from the in-use count without returning it to the idle list.
// Used when a sticky/reserved frontend session disconnects.
func (p *Pool) Discard(b Conn) error {
	if b == nil {
		return nil
	}
	err := b.Close()
	p.mu.Lock()
	if p.active > 0 {
		p.active--
	}
	delete(p.created, b)
	p.mu.Unlock()
	return err
}

// Close closes idle backends and stops background workers; in-use backends are
// closed when released.
func (p *Pool) Close() error {
	p.stopOnce.Do(func() { close(p.stop) })

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	idle := p.idle
	p.idle = nil
	for _, ic := range idle {
		delete(p.created, ic.conn)
	}
	p.mu.Unlock()

	var first error
	for _, ic := range idle {
		if err := ic.conn.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (p *Pool) expired(created time.Time) bool {
	return p.cfg.MaxLifetime > 0 && !created.IsZero() &&
		time.Since(created) >= p.cfg.MaxLifetime
}

func (p *Pool) needPing(since time.Time) bool {
	return p.cfg.HealthCheckInterval > 0 &&
		time.Since(since) >= p.cfg.HealthCheckInterval
}

// warmup best-effort opens connections up to Capacity.
func (p *Pool) warmup() {
	for {
		p.mu.Lock()
		if p.closed || len(p.idle) >= p.cfg.Capacity || p.liveLocked() >= p.cfg.MaxCapacity {
			p.mu.Unlock()
			return
		}
		p.active++
		open := p.open
		p.mu.Unlock()

		if open == nil {
			p.mu.Lock()
			p.active--
			p.mu.Unlock()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), p.cfg.GetTimeout)
		b, err := open(ctx)
		cancel()

		p.mu.Lock()
		p.active--
		if err != nil || b == nil {
			p.mu.Unlock()
			return
		}
		if p.closed || len(p.idle) >= p.cfg.MaxIdle {
			p.mu.Unlock()
			_ = b.Close()
			return
		}
		now := time.Now()
		p.created[b] = now
		p.idle = append(p.idle, idleConn{conn: b, since: now})
		p.mu.Unlock()
	}
}

func (p *Pool) reaper() {
	interval := time.Second
	if p.cfg.IdleTimeout > 0 && p.cfg.IdleTimeout/2 < interval {
		interval = p.cfg.IdleTimeout / 2
	}
	if p.cfg.MaxLifetime > 0 && p.cfg.MaxLifetime/2 < interval {
		interval = p.cfg.MaxLifetime / 2
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case now := <-ticker.C:
			p.reap(now)
		}
	}
}

func (p *Pool) reap(now time.Time) {
	p.mu.Lock()
	keep := p.idle[:0]
	var expired []Conn
	for _, ic := range p.idle {
		created := p.created[ic.conn]
		idleExpired := p.cfg.IdleTimeout > 0 && now.Sub(ic.since) >= p.cfg.IdleTimeout
		lifeExpired := p.cfg.MaxLifetime > 0 && !created.IsZero() &&
			now.Sub(created) >= p.cfg.MaxLifetime
		if idleExpired || lifeExpired {
			expired = append(expired, ic.conn)
			delete(p.created, ic.conn)
			continue
		}
		keep = append(keep, ic)
	}
	p.idle = keep
	p.mu.Unlock()

	for _, c := range expired {
		_ = c.Close()
	}
}
