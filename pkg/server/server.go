// Package server owns the proxy listener, accept loop, and per-connection
// command loop (serve.go, folded in from pkg/proxy — A15).
// API shape follows kingshard/Gaea: New → Run (blocking Accept) → Close;
// config hot-reload via UpdateConfig (caller re-reads YAML, same as kingshard main).
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acorndb/shardweave/pkg/backend"
	"github.com/acorndb/shardweave/pkg/config"
	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/acorndb/shardweave/pkg/execute"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/route/shard"
	"github.com/acorndb/shardweave/pkg/route/source"
)

// Server is one proxy process: listen address + executor + live config.
type Server struct {
	path string
	rt   *config.Runtime
	exec execute.Executor

	readTimeout      time.Duration
	writeTimeout     time.Duration
	maxAllowedPacket int
	maxConns         int
	sharding         bool
	activeConns      atomic.Int64
	connIDs          connIDAllocator

	listener net.Listener
	running  atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
}

// New builds a Server and binds the listen address.
func New(cfg *config.Config, configPath string) (*Server, error) {
	if cfg == nil {
		return nil, fmt.Errorf("server: nil config")
	}
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}

	rt := config.NewRuntime(cfg)
	exec, err := newExecutor(rt, cfg)
	if err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		_ = exec.Close()
		return nil, fmt.Errorf("server: listen %s: %w", cfg.Listen, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	mode := cfg.Backend.Mode
	if mode == "" {
		mode = constant.ModeSingle
	}
	s := &Server{
		path:             configPath,
		rt:               rt,
		exec:             exec,
		readTimeout:      cfg.Server.ReadTimeout,
		writeTimeout:     cfg.Server.WriteTimeout,
		maxAllowedPacket: cfg.Server.MaxAllowedPacket,
		maxConns:         cfg.Server.MaxConnections,
		sharding:         mode == constant.ModeSharding,
		listener:         ln,
		ctx:              ctx,
		cancel:           cancel,
	}
	s.running.Store(true)
	return s, nil
}

func newExecutor(rt *config.Runtime, cfg *config.Config) (execute.Executor, error) {
	pcfg := toPoolConfig(cfg.Pool)
	pcfg.MaxAllowedPacket = cfg.Server.MaxAllowedPacket
	bs := cfg.Backend
	mode := bs.Mode
	if mode == "" {
		mode = constant.ModeSingle
	}
	_ = rt // pools dial from cfg snapshot at open; address reload still rejected

	switch mode {
	case constant.ModeSingle:
		if len(bs.Slices) != 1 {
			return nil, fmt.Errorf("server: single requires exactly 1 slice")
		}
		cluster, err := buildCluster(bs, pcfg)
		if err != nil {
			return nil, err
		}
		exec, err := execute.New(cluster, plan.Builder{Router: source.AllMaster{Slice: bs.Slices[0].Name}})
		if err != nil {
			_ = cluster.Close()
			return nil, err
		}
		return exec, nil
	case constant.ModeRWSplit:
		if len(bs.Slices) != 1 {
			return nil, fmt.Errorf("server: rwsplit requires exactly 1 slice")
		}
		cluster, err := buildCluster(bs, pcfg)
		if err != nil {
			return nil, err
		}
		exec, err := execute.New(cluster, plan.Builder{Router: source.Pick{Slice: bs.Slices[0].Name}})
		if err != nil {
			_ = cluster.Close()
			return nil, err
		}
		return exec, nil
	case constant.ModeSharding:
		cluster, err := buildCluster(bs, pcfg)
		if err != nil {
			return nil, err
		}
		rules := make([]shard.Rule, 0, len(bs.Sharding.Rules))
		for _, r := range bs.Sharding.Rules {
			rules = append(rules, shard.Rule{
				Table:     r.Table,
				Key:       r.Key,
				Algorithm: r.Algorithm,
				Slices:    r.Slices,
			})
		}
		router, err := shard.New(rules)
		if err != nil {
			_ = cluster.Close()
			return nil, err
		}
		exec, err := execute.New(cluster, plan.Builder{Router: router, Sharding: true})
		if err != nil {
			_ = cluster.Close()
			return nil, err
		}
		return exec, nil
	default:
		return nil, fmt.Errorf("server: unsupported backend.mode %q", mode)
	}
}

// buildCluster builds one Slice per configured endpoint group. single/rwsplit
// pass exactly one slice; sharding will pass N. Pools are lazy, so this does
// not dial until the first Acquire.
func buildCluster(bs config.BackendConfig, pcfg backend.PoolConfig) (*backend.Cluster, error) {
	open := func(ep config.BackendEndpoint) backend.OpenFunc {
		return func(ctx context.Context) (backend.Conn, error) {
			return backend.DialMySQLWithOptions(ctx, ep.Address, ep.User, ep.Password, ep.Database,
				backend.DialOptions{
					DialTimeout:      pcfg.DialTimeout,
					ReadTimeout:      pcfg.ReadTimeout,
					WriteTimeout:     pcfg.WriteTimeout,
					MaxAllowedPacket: pcfg.MaxAllowedPacket,
				})
		}
	}
	lb := bs.LoadBalance
	if lb == "" {
		lb = constant.LoadBalanceRoundRobin
	}
	slices := make([]*backend.Slice, 0, len(bs.Slices))
	for i := range bs.Slices {
		s := bs.Slices[i]
		slice := &backend.Slice{
			Name:        s.Name,
			Master:      backend.NewPool(pcfg, open(s.Master)),
			LoadBalance: lb,
		}
		for j := range s.Slaves {
			slice.Slaves = append(slice.Slaves, backend.NewPool(pcfg, open(s.Slaves[j])))
		}
		slices = append(slices, slice)
	}
	cluster, err := backend.NewCluster(slices...)
	if err != nil {
		for _, s := range slices {
			_ = s.Close()
		}
		return nil, err
	}
	return cluster, nil
}

// Addr returns the bound listen address.
func (s *Server) Addr() string {
	if s == nil || s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Runtime exposes the live config snapshot.
func (s *Server) Runtime() *config.Runtime {
	return s.rt
}

// ConfigPath returns the YAML path used for reloads.
func (s *Server) ConfigPath() string {
	return s.path
}

// Run accepts connections until Close.
func (s *Server) Run() error {
	for s.running.Load() {
		conn, err := s.listener.Accept()
		if err != nil {
			if !s.running.Load() {
				return nil
			}
			slog.Error("server accept", "err", err)
			continue
		}
		if !s.acquireConn() {
			slog.Warn("server reached max connections", "remote", conn.RemoteAddr().String(), "max", s.maxConns)
			go rejectTooManyConnections(conn, s.writeTimeout)
			continue
		}
		go s.onConn(conn)
	}
	return nil
}

// acquireConn reserves one frontend session slot. In production only the
// accept loop calls it, so the counter has a single incrementer and closers
// only decrement. The CAS loop is bounded; an exhausted budget falls back to
// reserve-and-validate, so false always means the limit is reached.
func (s *Server) acquireConn() bool {
	if s.maxConns <= 0 {
		s.activeConns.Add(1)
		return true
	}
	max := int64(s.maxConns)
	for i := 0; i < constant.MaxAcquireCASRounds; i++ {
		cur := s.activeConns.Load()
		if cur >= max {
			return false
		}
		if s.activeConns.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
	// Retry budget exhausted: reserve once and validate instead of spinning.
	// false here still means the limit is reached, not that the CAS lost.
	if n := s.activeConns.Add(1); n > max {
		s.activeConns.Add(-1)
		return false
	}
	return true
}

func (s *Server) releaseConn() {
	s.activeConns.Add(-1)
}

// ActiveConnections returns the number of accepted frontend sessions.
func (s *Server) ActiveConnections() int {
	if s == nil {
		return 0
	}
	return int(s.activeConns.Load())
}

func (s *Server) onConn(c net.Conn) {
	defer s.releaseConn()
	id := s.connIDs.alloc()
	defer s.connIDs.free(id)
	defer func() {
		if err := recover(); err != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			slog.Error("server onConn panic", "conn_id", id, "remote", c.RemoteAddr().String(), "err", err, "stack", string(buf[:n]))
		}
		_ = c.Close()
	}()

	_ = Serve(s.ctx, c, SessionConfig{
		Users:            s.rt.UserProvider(),
		Exec:             s.exec,
		ReadTimeout:      s.readTimeout,
		WriteTimeout:     s.writeTimeout,
		MaxAllowedPacket: s.maxAllowedPacket,
		Sharding:         s.sharding,
		ConnectionID:     id,
	})
}

// UpdateConfig applies an already-parsed config (kingshard UpdateConfig).
// listen / backend topology changes are rejected by config.Runtime.Apply.
func (s *Server) UpdateConfig(cfg *config.Config) error {
	return s.rt.Apply(cfg)
}

// Close stops Accept and releases pools.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.running.Store(false)
		if s.cancel != nil {
			s.cancel()
		}
		if s.listener != nil {
			_ = s.listener.Close()
		}
		if s.exec != nil {
			_ = s.exec.Close()
		}
	})
}

func toPoolConfig(c config.Pool) backend.PoolConfig {
	return backend.PoolConfig{
		Capacity:            c.Capacity,
		MaxCapacity:         c.MaxCapacity,
		MaxIdle:             c.MaxIdle,
		IdleTimeout:         c.IdleTimeout,
		MaxLifetime:         c.MaxLifetime,
		HealthCheckInterval: c.HealthCheckInterval,
		AcquireTimeout:      c.AcquireTimeout,
		DialTimeout:         c.DialTimeout,
		ReadTimeout:         c.ReadTimeout,
		WriteTimeout:        c.WriteTimeout,
	}
}
