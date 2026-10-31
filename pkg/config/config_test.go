package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acorndb/prism/pkg/constant"
	"github.com/stretchr/testify/require"
)

func TestLoadFile_OK_Single(t *testing.T) {
	path := writeYAML(t, `
listen: "127.0.0.1:3307"
users:
  - name: app
    password: secret
backend_server:
  mode: single
  address: "127.0.0.1:3306"
  user: app
  password: secret
  database: appdb
pool:
  capacity: 2
  max_capacity: 8
  get_timeout: 3s
log:
  level: info
termination_drain: 2s
`)
	cfg, err := LoadFile(path)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:3307", cfg.Listen)
	require.Len(t, cfg.Users, 1)
	require.Equal(t, constant.ModeSingle, cfg.BackendServer.Mode)
	ep, err := cfg.BackendServer.Primary()
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:3306", ep.Address)
	require.Equal(t, "app", ep.User)
	require.Equal(t, "appdb", ep.Database)
	require.Equal(t, 2, cfg.Pool.Capacity)
	require.Equal(t, 8, cfg.Pool.MaxCapacity)
	require.Equal(t, 3*time.Second, cfg.Pool.GetTimeout)
	require.Equal(t, "info", cfg.Log.Level)
	require.Equal(t, 2*time.Second, cfg.TerminationDrain)
}

func TestLoadFile_ModeDefaultSingle(t *testing.T) {
	path := writeYAML(t, `
listen: "127.0.0.1:3307"
users:
  - name: app
    password: secret
backend_server:
  address: "127.0.0.1:3306"
  user: app
  password: secret
`)
	cfg, err := LoadFile(path)
	require.NoError(t, err)
	require.Equal(t, constant.ModeSingle, cfg.BackendServer.Mode)
}

func TestLoadFile_OK_RWSplit(t *testing.T) {
	path := writeYAML(t, `
listen: "127.0.0.1:3307"
users:
  - name: app
    password: secret
backend_server:
  mode: rwsplit
  master:
    address: "127.0.0.1:3306"
    user: app
    password: secret
  slaves:
    - address: "127.0.0.1:3307"
      user: app
      password: secret
  load_balance: round_robin
`)
	cfg, err := LoadFile(path)
	require.NoError(t, err)
	require.Equal(t, constant.ModeRWSplit, cfg.BackendServer.Mode)
	require.Equal(t, "127.0.0.1:3306", cfg.BackendServer.Master.Address)
	require.Len(t, cfg.BackendServer.Slaves, 1)
	require.Equal(t, constant.LoadBalanceRoundRobin, cfg.BackendServer.LoadBalance)
}

func TestLoadFile_RejectOldBackendKey(t *testing.T) {
	path := writeYAML(t, `
listen: "127.0.0.1:3307"
users:
  - name: app
    password: secret
backend:
  address: "127.0.0.1:3306"
  user: app
  password: secret
`)
	_, err := LoadFile(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "backend")
}

func TestLoadFile_RejectUnknownField(t *testing.T) {
	path := writeYAML(t, `
listen: "127.0.0.1:3307"
users:
  - name: app
    password: secret
backend_server:
  mode: single
  address: "127.0.0.1:3306"
  user: app
  password: secret
not_a_real_field: true
`)
	_, err := LoadFile(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not_a_real_field")
}

func TestValidate_RejectShardingAndUnknownMode(t *testing.T) {
	base := validSingle()
	base.BackendServer.Mode = constant.ModeSharding
	require.Error(t, Validate(base))

	base = validSingle()
	base.BackendServer.Mode = "weird"
	require.Error(t, Validate(base))
}

func TestValidate_RWSplitRequiresMaster(t *testing.T) {
	cfg := validSingle()
	cfg.BackendServer = BackendServer{Mode: constant.ModeRWSplit}
	require.Error(t, Validate(cfg))

	cfg.BackendServer.Master = &BackendEndpoint{Address: "127.0.0.1:3306", User: "a", Password: "b"}
	require.NoError(t, Validate(cfg))
}

func TestValidate_SingleRejectsMasterBlock(t *testing.T) {
	cfg := validSingle()
	cfg.BackendServer.Master = &BackendEndpoint{Address: "x", User: "a"}
	require.Error(t, Validate(cfg))
}

func TestValidate_RequiresListenUsersBackendServer(t *testing.T) {
	err := Validate(&Config{})
	require.Error(t, err)

	require.NoError(t, Validate(validSingle()))
}

func TestRuntime_ReloadUsers(t *testing.T) {
	base := validSingle()
	base.Users = []User{{Name: "a", Password: "old"}}
	rt := NewRuntime(base)
	require.Equal(t, "old", rt.Load().Users[0].Password)

	next := cloneConfig(base)
	next.Users = []User{{Name: "a", Password: "new"}}
	require.NoError(t, rt.Apply(next))
	require.Equal(t, "new", rt.Load().Users[0].Password)
}

func TestRuntime_RejectListenChange(t *testing.T) {
	base := validSingle()
	rt := NewRuntime(base)
	next := cloneConfig(base)
	next.Listen = "127.0.0.1:3308"
	err := rt.Apply(next)
	require.Error(t, err)
	require.Contains(t, err.Error(), "listen")
	require.Equal(t, "127.0.0.1:3307", rt.Load().Listen)
}

func TestRuntime_RejectBackendServerChange(t *testing.T) {
	base := validSingle()
	rt := NewRuntime(base)
	next := cloneConfig(base)
	next.BackendServer.Address = "10.0.0.1:3306"
	err := rt.Apply(next)
	require.Error(t, err)
	require.Contains(t, err.Error(), "backend_server")
}

func TestRuntime_ReloadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prism.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
listen: "127.0.0.1:3307"
users:
  - name: app
    password: p1
backend_server:
  mode: single
  address: "127.0.0.1:3306"
  user: app
  password: p1
`), 0o600))
	cfg, err := LoadFile(path)
	require.NoError(t, err)
	rt := NewRuntime(cfg)

	require.NoError(t, os.WriteFile(path, []byte(`
listen: "127.0.0.1:3307"
users:
  - name: app
    password: p2
backend_server:
  mode: single
  address: "127.0.0.1:3306"
  user: app
  password: p1
`), 0o600))
	require.NoError(t, rt.ReloadFile(path))
	require.Equal(t, "p2", rt.Load().Users[0].Password)

	require.NoError(t, os.WriteFile(path, []byte("listen: [\n"), 0o600))
	require.Error(t, rt.ReloadFile(path))
	require.Equal(t, "p2", rt.Load().Users[0].Password)
}

func TestRuntime_UserProvider(t *testing.T) {
	rt := NewRuntime(validSingle())
	users := rt.UserProvider()
	pass, ok := users.Password("app")
	require.True(t, ok)
	require.Equal(t, "secret", pass)

	next := rt.Load().Clone()
	next.Users = []User{{Name: "app", Password: "p2"}}
	require.NoError(t, rt.Apply(next))
	pass, ok = users.Password("app")
	require.True(t, ok)
	require.Equal(t, "p2", pass)
}

func validSingle() *Config {
	return &Config{
		Listen: "127.0.0.1:3307",
		Users:  []User{{Name: "app", Password: "secret"}},
		BackendServer: BackendServer{
			Mode:     constant.ModeSingle,
			Address:  "127.0.0.1:3306",
			User:     "app",
			Password: "secret",
			Database: "appdb",
		},
		Log: Log{Level: "info"},
	}
}

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prism.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func cloneConfig(c *Config) *Config {
	return c.Clone()
}
