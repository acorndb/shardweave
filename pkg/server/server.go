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

	"github.com/acorndb/prism/pkg/config"
	"github.com/acorndb/prism/pkg/pool"
	"github.com/acorndb/prism/pkg/proxy"
)

// Server is one proxy process: listen address + backend pool + live config.
type Server struct {
	path string
	rt   *config.Runtime
	pool *pool.Pool

	listener net.Listener
	running  atomic.Bool

	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
}

// New builds a Server and binds the listen address (kingshard NewServer).
func New(cfg *config.Config, configPath string) (*Server, error) {
	if cfg == nil {
		return nil, fmt.Errorf("server: nil config")
	}
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}

	rt := config.NewRuntime(cfg)
	p := pool.New(toPoolConfig(cfg.Pool), func(ctx context.Context) (pool.Backend, error) {
		b := rt.Load().Backend
		return pool.DialMySQL(ctx, b.Address, b.User, b.Password, b.Database)
	})

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("server: listen %s: %w", cfg.Listen, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		path:     configPath,
		rt:       rt,
		pool:     p,
		listener: ln,
		ctx:      ctx,
		cancel:   cancel,
	}
	s.running.Store(true)
	return s, nil
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

// Run accepts connections until Close (kingshard/Gaea Server.Run).
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
		Users: s.rt.UserProvider(),
		Pool:  s.pool,
	})
}

// UpdateConfig applies an already-parsed config (kingshard UpdateConfig).
// listen / backend identity changes are rejected by config.Runtime.Apply.
func (s *Server) UpdateConfig(cfg *config.Config) error {
	return s.rt.Apply(cfg)
}

// Close stops Accept and releases the pool (kingshard/Gaea Close).
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.running.Store(false)
		if s.cancel != nil {
			s.cancel()
		}
		if s.listener != nil {
			_ = s.listener.Close()
		}
		if s.pool != nil {
			_ = s.pool.Close()
		}
	})
}

func toPoolConfig(c config.Pool) pool.Config {
	return pool.Config{
		Capacity:    c.Capacity,
		MaxCapacity: c.MaxCapacity,
		IdleTimeout: c.IdleTimeout,
		GetTimeout:  c.GetTimeout,
	}
}
