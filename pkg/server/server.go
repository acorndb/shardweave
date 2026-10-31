// Package server owns the proxy listener and accept loop.
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

	"github.com/acorndb/prism/pkg/backend"
	"github.com/acorndb/prism/pkg/config"
	"github.com/acorndb/prism/pkg/constant"
	"github.com/acorndb/prism/pkg/execute"
	"github.com/acorndb/prism/pkg/plan"
	"github.com/acorndb/prism/pkg/proxy"
	"github.com/acorndb/prism/pkg/route/source"
)

// Server is one proxy process: listen address + executor + live config.
type Server struct {
	path string
	rt   *config.Runtime
	exec execute.Executor

	readTimeout      time.Duration
	writeTimeout     time.Duration
	maxAllowedPacket int

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
	s := &Server{
		path:             configPath,
		rt:               rt,
		exec:             exec,
		readTimeout:      cfg.Server.ReadTimeout,
		writeTimeout:     cfg.Server.WriteTimeout,
		maxAllowedPacket: cfg.Server.MaxAllowedPacket,
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

	var g *backend.Slice
	var builder plan.Builder
	switch mode {
	case constant.ModeSingle:
		if len(bs.Slices) != 1 {
			return nil, fmt.Errorf("server: single requires exactly 1 slice")
		}
		s := bs.Slices[0]
		g = &backend.Slice{Name: s.Name, Master: backend.NewPool(pcfg, open(s.Master))}
		builder = plan.Builder{Router: source.AllMaster{Slice: s.Name}}
	case constant.ModeRWSplit:
		if len(bs.Slices) != 1 {
			return nil, fmt.Errorf("server: rwsplit requires exactly 1 slice")
		}
		s := bs.Slices[0]
		g = &backend.Slice{
			Name:        s.Name,
			Master:      backend.NewPool(pcfg, open(s.Master)),
			LoadBalance: bs.LoadBalance,
		}
		if g.LoadBalance == "" {
			g.LoadBalance = constant.LoadBalanceRoundRobin
		}
		for i := range s.Slaves {
			g.Slaves = append(g.Slaves, backend.NewPool(pcfg, open(s.Slaves[i])))
		}
		builder = plan.Builder{Router: source.Pick{Slice: s.Name}}
		_ = rt // pools dial from cfg snapshot at open; address reload still rejected
	default:
		return nil, fmt.Errorf("server: unsupported backend.mode %q", mode)
	}
	cluster, err := backend.NewCluster(g)
	if err != nil {
		return nil, err
	}
	return execute.New(cluster, builder)
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
		go s.onConn(conn)
	}
	return nil
}

func (s *Server) onConn(c net.Conn) {
	defer func() {
		if err := recover(); err != nil {
			buf := make([]byte, 4096)
			n := runtime.Stack(buf, false)
			slog.Error("server onConn panic", "remote", c.RemoteAddr().String(), "err", err, "stack", string(buf[:n]))
		}
		_ = c.Close()
	}()

	_ = proxy.Serve(s.ctx, c, proxy.SessionConfig{
		Users:            s.rt.UserProvider(),
		Exec:             s.exec,
		ReadTimeout:      s.readTimeout,
		WriteTimeout:     s.writeTimeout,
		MaxAllowedPacket: s.maxAllowedPacket,
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
