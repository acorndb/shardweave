package server

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acorndb/shardweave/pkg/backend"
	"github.com/acorndb/shardweave/pkg/config"
	"github.com/acorndb/shardweave/pkg/constant"
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
	require.NoError(t, c2.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = c2.Read(make([]byte, 1))
	require.Error(t, err, "second connection must be rejected")

	require.NoError(t, c1.Close())
	require.Eventually(t, func() bool { return srv.ActiveConnections() == 0 }, time.Second, time.Millisecond)
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
