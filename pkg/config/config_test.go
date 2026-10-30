package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadFile_OK(t *testing.T) {
	path := writeYAML(t, `
listen: "127.0.0.1:3307"
users:
  - name: app
    password: secret
backend:
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
	require.Equal(t, "app", cfg.Users[0].Name)
	require.Equal(t, "secret", cfg.Users[0].Password)
	require.Equal(t, "127.0.0.1:3306", cfg.Backend.Address)
	require.Equal(t, "app", cfg.Backend.User)
	require.Equal(t, "appdb", cfg.Backend.Database)
	require.Equal(t, 2, cfg.Pool.Capacity)
	require.Equal(t, 8, cfg.Pool.MaxCapacity)
	require.Equal(t, 3*time.Second, cfg.Pool.GetTimeout)
	require.Equal(t, "info", cfg.Log.Level)
	require.Equal(t, 2*time.Second, cfg.TerminationDrain)
}

func TestLoadFile_RejectUnknownField(t *testing.T) {
	path := writeYAML(t, `
listen: "127.0.0.1:3307"
users:
  - name: app
    password: secret
backend:
  address: "127.0.0.1:3306"
  user: app
  password: secret
not_a_real_field: true
`)
	_, err := LoadFile(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not_a_real_field")
}

func TestValidate_RequiresListenUsersBackend(t *testing.T) {
	err := Validate(&Config{})
	require.Error(t, err)

	err = Validate(&Config{
		Listen: "127.0.0.1:3307",
		Users:  []User{{Name: "a", Password: "b"}},
		Backend: Backend{
			Address:  "127.0.0.1:3306",
			User:     "a",
			Password: "b",
		},
	})
	require.NoError(t, err)
}

func TestRuntime_ReloadUsers(t *testing.T) {
	base := &Config{
		Listen: "127.0.0.1:3307",
		Users:  []User{{Name: "a", Password: "old"}},
		Backend: Backend{
			Address:  "127.0.0.1:3306",
			User:     "a",
			Password: "b",
		},
		Log: Log{Level: "info"},
	}
	rt := NewRuntime(base)
	cur := rt.Load()
	require.Equal(t, "old", cur.Users[0].Password)

	next := cloneConfig(base)
	next.Users = []User{{Name: "a", Password: "new"}}
	require.NoError(t, rt.Apply(next))
	require.Equal(t, "new", rt.Load().Users[0].Password)
}

func TestRuntime_RejectListenChange(t *testing.T) {
	base := &Config{
		Listen: "127.0.0.1:3307",
		Users:  []User{{Name: "a", Password: "x"}},
		Backend: Backend{
			Address:  "127.0.0.1:3306",
			User:     "a",
			Password: "b",
		},
	}
	rt := NewRuntime(base)
	next := cloneConfig(base)
	next.Listen = "127.0.0.1:3308"
	err := rt.Apply(next)
	require.Error(t, err)
	require.Contains(t, err.Error(), "listen")
	require.Equal(t, "127.0.0.1:3307", rt.Load().Listen)
}

func TestRuntime_RejectBackendChange(t *testing.T) {
	base := &Config{
		Listen: "127.0.0.1:3307",
		Users:  []User{{Name: "a", Password: "x"}},
		Backend: Backend{
			Address:  "127.0.0.1:3306",
			User:     "a",
			Password: "b",
		},
	}
	rt := NewRuntime(base)
	next := cloneConfig(base)
	next.Backend.Address = "10.0.0.1:3306"
	err := rt.Apply(next)
	require.Error(t, err)
	require.Contains(t, err.Error(), "backend")
}

func TestRuntime_ReloadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prism.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
listen: "127.0.0.1:3307"
users:
  - name: app
    password: p1
backend:
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
backend:
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
	rt := NewRuntime(&Config{
		Listen: "127.0.0.1:3307",
		Users:  []User{{Name: "app", Password: "p1"}},
		Backend: Backend{
			Address:  "127.0.0.1:3306",
			User:     "app",
			Password: "p1",
		},
	})
	users := rt.UserProvider()
	pass, ok := users.Password("app")
	require.True(t, ok)
	require.Equal(t, "p1", pass)

	next := rt.Load().Clone()
	next.Users = []User{{Name: "app", Password: "p2"}}
	require.NoError(t, rt.Apply(next))
	pass, ok = users.Password("app")
	require.True(t, ok)
	require.Equal(t, "p2", pass)
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
