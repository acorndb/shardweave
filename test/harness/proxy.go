package harness

import (
	"fmt"
	"sync"
	"time"

	"github.com/acorndb/shardweave/pkg/config"
	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/acorndb/shardweave/pkg/server"
)

// Proxy is an in-process shardweave server bound to a loopback port.
type Proxy struct {
	srv  *server.Server
	addr string

	closeOnce sync.Once
}

// ProxyConfig builds an rwsplit config pointing at the real topology.
func (tp *Topology) ProxyConfig() *config.Config {
	slice := config.SliceConfig{
		Name: "default",
		Master: config.BackendEndpoint{
			Address:  tp.Master.Addr(),
			User:     AppUser,
			Password: AppPassword,
			Database: Database,
		},
	}
	for _, r := range tp.Replicas {
		slice.Slaves = append(slice.Slaves, config.BackendEndpoint{
			Address:  r.Addr(),
			User:     AppUser,
			Password: AppPassword,
			Database: Database,
		})
	}
	return &config.Config{
		Listen: "127.0.0.1:0",
		Users:  []config.User{{Name: AppUser, Password: AppPassword}},
		Server: config.Server{MaxAllowedPacket: 0},
		Backend: config.BackendConfig{
			Mode:        constant.ModeRWSplit,
			Slices:      []config.SliceConfig{slice},
			LoadBalance: constant.LoadBalanceRoundRobin,
		},
		Pool: config.Pool{
			Capacity:       1,
			MaxCapacity:    8,
			MaxIdle:        4,
			AcquireTimeout: 5 * time.Second,
			DialTimeout:    3 * time.Second,
			ReadTimeout:    15 * time.Second,
			WriteTimeout:   15 * time.Second,
		},
	}
}

// StartProxy starts the proxy and its accept loop.
func StartProxy(cfg *config.Config) (*Proxy, error) {
	srv, err := server.New(cfg, "")
	if err != nil {
		return nil, fmt.Errorf("harness: start proxy: %w", err)
	}
	p := &Proxy{srv: srv, addr: srv.Addr()}
	go func() {
		_ = srv.Run()
	}()
	return p, nil
}

// Addr returns the frontend listen address.
func (p *Proxy) Addr() string { return p.addr }

// DSN returns a client DSN for the proxy frontend.
func (p *Proxy) DSN() string { return appDSN(p.addr, Database) }

// Close stops the listener and releases backend pools.
func (p *Proxy) Close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		p.srv.Close()
	})
}
