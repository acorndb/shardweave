package server

import (
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acorndb/shardweave/pkg/backend"
	"github.com/acorndb/shardweave/pkg/config"
	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/auth"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/stretchr/testify/require"
)

func TestServer_RunExitsOnClose(t *testing.T) {
	cfg := minimalConfig()
	path := writeConfig(t, cfg)

	srv, err := New(cfg, path)
	require.NoError(t, err)
	require.NotEmpty(t, srv.Addr())

	done := make(chan struct{})
	go func() {
		_ = srv.Run()
		close(done)
	}()

	srv.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Close")
	}
}

func TestServer_MaxConnections(t *testing.T) {
	cfg := minimalConfig()
	cfg.Server.MaxConnections = 1
	path := writeConfig(t, cfg)

	srv, err := New(cfg, path)
	require.NoError(t, err)
	t.Cleanup(srv.Close)
	go func() { _ = srv.Run() }()

	c1, err := net.Dial("tcp", srv.Addr())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c1.Close() })
	require.Eventually(t, func() bool { return srv.ActiveConnections() == 1 }, time.Second, time.Millisecond)

	c2, err := net.Dial("tcp", srv.Addr())
	require.NoError(t, err)
	defer func() { _ = c2.Close() }()
	require.NoError(t, c2.SetReadDeadline(time.Now().Add(2*time.Second)))
	data, err := packet.NewConn(c2).ReadPacket()
	require.NoError(t, err)
	require.Equal(t, byte(mysql.ErrHeader), data[0], "rejection must be an ERR packet, not a handshake")
	var me *errs.MySQL
	require.ErrorAs(t, mysql.DecodeServerError(data), &me)
	require.Equal(t, uint16(errs.ErTooManyConnections), me.Number)
	require.Equal(t, errs.StateTooManyConns, me.SQLState)

	require.NoError(t, c1.Close())
	require.Eventually(t, func() bool { return srv.ActiveConnections() == 0 }, time.Second, time.Millisecond)
}

// TestServer_ConnectionIDsAreUnique pins the frontend handshake contract: every
// accepted session gets its own MySQL connection id, starting at 1.
func TestServer_ConnectionIDsAreUnique(t *testing.T) {
	cfg := minimalConfig()
	srv, err := New(cfg, writeConfig(t, cfg))
	require.NoError(t, err)
	t.Cleanup(srv.Close)
	go func() { _ = srv.Run() }()

	readID := func() uint32 {
		c, err := net.Dial("tcp", srv.Addr())
		require.NoError(t, err)
		defer func() { _ = c.Close() }()
		data, err := packet.NewConn(c).ReadPacket()
		require.NoError(t, err)
		hs, err := auth.ParseHandshakeV10(data)
		require.NoError(t, err)
		return hs.ConnectionID
	}

	require.Equal(t, uint32(1), readID())
	require.Equal(t, uint32(2), readID())
}

func TestServer_AcquireConn(t *testing.T) {
	tests := []struct {
		name      string
		max       int
		pre       int64
		want      bool
		wantCount int64
	}{
		{name: "unlimited", max: 0, want: true, wantCount: 1},
		{name: "below_limit", max: 2, want: true, wantCount: 1},
		{name: "at_limit", max: 2, pre: 2, want: false, wantCount: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{maxConns: tc.max}
			s.activeConns.Store(tc.pre)
			require.Equal(t, tc.want, s.acquireConn())
			require.Equal(t, tc.wantCount, s.activeConns.Load())
		})
	}
}

func TestServer_ReleaseConnFreesSlot(t *testing.T) {
	s := &Server{maxConns: 1}
	require.True(t, s.acquireConn())
	require.False(t, s.acquireConn(), "limit reached")
	s.releaseConn()
	require.True(t, s.acquireConn(), "released slot must be reusable")
}

func TestServer_AcquireConnConcurrentChurn(t *testing.T) {
	const (
		maxSlots = 4
		workers  = 16
		rounds   = 500
	)
	s := &Server{maxConns: maxSlots}
	var admitted, over atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				if !s.acquireConn() {
					continue
				}
				if n := admitted.Add(1); n > maxSlots {
					over.Store(n)
				}
				admitted.Add(-1)
				s.releaseConn()
			}
		}()
	}
	wg.Wait()
	require.Zero(t, over.Load(), "acquireConn over-admitted slots")
	require.Zero(t, s.activeConns.Load(), "slots leaked")
}

func TestServer_UpdateConfigUsers(t *testing.T) {
	cfg := minimalConfig()
	path := writeConfig(t, cfg)
	srv, err := New(cfg, path)
	require.NoError(t, err)
	t.Cleanup(srv.Close)

	pass, ok := srv.Runtime().UserProvider().Password("app")
	require.True(t, ok)
	require.Equal(t, "p1", pass)

	next, err := config.LoadFile(path)
	require.NoError(t, err)
	next.Users = []config.User{{Name: "app", Password: "p2"}}
	require.NoError(t, srv.UpdateConfig(next))

	pass, ok = srv.Runtime().UserProvider().Password("app")
	require.True(t, ok)
	require.Equal(t, "p2", pass)
}

func TestServer_UpdateConfigRejectsListenChange(t *testing.T) {
	cfg := minimalConfig()
	path := writeConfig(t, cfg)
	srv, err := New(cfg, path)
	require.NoError(t, err)
	t.Cleanup(srv.Close)

	next := cfg.Clone()
	next.Listen = "127.0.0.1:19999"
	err = srv.UpdateConfig(next)
	require.Error(t, err)
	require.Contains(t, err.Error(), "listen")
}

func TestNewExecutor_RWSplit(t *testing.T) {
	cfg := minimalConfig()
	cfg.Backend = config.BackendConfig{
		Mode: constant.ModeRWSplit,
		Slices: []config.SliceConfig{{
			Name:   "default",
			Master: config.BackendEndpoint{Address: "127.0.0.1:3306", User: "app", Password: "p1"},
			Slaves: []config.BackendEndpoint{{Address: "127.0.0.1:3307", User: "app", Password: "p1"}},
		}},
	}
	require.NoError(t, config.Validate(cfg))

	exec, err := newExecutor(nil, cfg)
	require.NoError(t, err)
	require.NoError(t, exec.Close())
}

func TestToPoolConfig(t *testing.T) {
	got := toPoolConfig(config.Pool{
		Capacity:            1,
		MaxCapacity:         2,
		MaxIdle:             1,
		IdleTimeout:         time.Minute,
		MaxLifetime:         time.Hour,
		HealthCheckInterval: 30 * time.Second,
		AcquireTimeout:      2 * time.Second,
		DialTimeout:         5 * time.Second,
		ReadTimeout:         15 * time.Second,
		WriteTimeout:        16 * time.Second,
	})
	require.Equal(t, backend.PoolConfig{
		Capacity:            1,
		MaxCapacity:         2,
		MaxIdle:             1,
		IdleTimeout:         time.Minute,
		MaxLifetime:         time.Hour,
		HealthCheckInterval: 30 * time.Second,
		AcquireTimeout:      2 * time.Second,
		DialTimeout:         5 * time.Second,
		ReadTimeout:         15 * time.Second,
		WriteTimeout:        16 * time.Second,
	}, got)
}

func minimalConfig() *config.Config {
	return &config.Config{
		Listen: "127.0.0.1:0",
		Users:  []config.User{{Name: "app", Password: "p1"}},
		Backend: config.BackendConfig{
			Mode: constant.ModeSingle,
			Slices: []config.SliceConfig{{
				Name: "default",
				Master: config.BackendEndpoint{
					Address:  "127.0.0.1:3306",
					User:     "app",
					Password: "p1",
				},
			}},
		},
		Log: config.Log{Level: "error"},
	}
}

func writeConfig(t *testing.T, cfg *config.Config) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "shardweave.yaml")
	body := "listen: \"" + cfg.Listen + "\"\n" +
		"users:\n  - name: app\n    password: p1\n" +
		"backend:\n  mode: single\n  slices:\n    - name: default\n" +
		"      master:\n        address: \"127.0.0.1:3306\"\n        user: app\n        password: p1\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func testEndpoint(addr string) config.BackendEndpoint {
	return config.BackendEndpoint{Address: addr, User: "app", Password: "p1"}
}

func TestBuildCluster_MultiSlice(t *testing.T) {
	bs := config.BackendConfig{
		LoadBalance: constant.LoadBalanceRandom,
		Slices: []config.SliceConfig{
			{Name: "s0", Master: testEndpoint("127.0.0.1:3306"), Slaves: []config.BackendEndpoint{testEndpoint("127.0.0.1:3406")}},
			{Name: "s1", Master: testEndpoint("127.0.0.1:3307")},
			{Name: "s2", Master: testEndpoint("127.0.0.1:3308")},
		},
	}
	pcfg := backend.PoolConfig{Capacity: 0, MaxCapacity: 1, AcquireTimeout: time.Second}

	cluster, err := buildCluster(bs, pcfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cluster.Close() })

	require.Equal(t, []string{"s0", "s1", "s2"}, cluster.Names())
	s0, err := cluster.Get("s0")
	require.NoError(t, err)
	require.Equal(t, constant.LoadBalanceRandom, s0.LoadBalance)
	require.Len(t, s0.Slaves, 1)
}

func TestBuildCluster_DefaultLoadBalance(t *testing.T) {
	bs := config.BackendConfig{Slices: []config.SliceConfig{
		{Name: "s0", Master: testEndpoint("127.0.0.1:3306")},
	}}
	cluster, err := buildCluster(bs, backend.PoolConfig{Capacity: 0, MaxCapacity: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cluster.Close() })

	s0, err := cluster.Get("s0")
	require.NoError(t, err)
	require.Equal(t, constant.LoadBalanceRoundRobin, s0.LoadBalance)
}

func TestNewExecutor_Sharding(t *testing.T) {
	cfg := minimalConfig()
	cfg.Backend = config.BackendConfig{
		Mode: constant.ModeSharding,
		Slices: []config.SliceConfig{
			{Name: "s0", Master: config.BackendEndpoint{Address: "127.0.0.1:3306", User: "app", Password: "p1"}},
			{Name: "s1", Master: config.BackendEndpoint{Address: "127.0.0.1:3307", User: "app", Password: "p1"}},
		},
		Sharding: &config.ShardingConfig{Rules: []config.ShardRule{{
			Table: "orders", Key: "user_id", Algorithm: constant.ShardingHashMod, Slices: []string{"s0", "s1"},
		}}},
	}
	require.NoError(t, config.Validate(cfg))

	exec, err := newExecutor(nil, cfg)
	require.NoError(t, err)
	require.NoError(t, exec.Close())
}
