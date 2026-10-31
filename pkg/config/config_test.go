package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acorndb/prism/pkg/constant"
	"github.com/stretchr/testify/require"
)

const singleYAML = `
listen: "127.0.0.1:3307"
users:
  - name: app
    password: secret
backend:
  mode: single
  slices:
    - name: default
      master:
        address: "127.0.0.1:3306"
        user: app
        password: secret
        database: appdb
server:
  read_timeout: 20s
  write_timeout: 21s
  max_allowed_packet: 8388608
pool:
  capacity: 2
  max_capacity: 8
  max_idle: 6
  idle_timeout: 5m
  max_lifetime: 1h
  health_check_interval: 30s
  acquire_timeout: 3s
  dial_timeout: 4s
  read_timeout: 15s
  write_timeout: 16s
log:
  level: info
termination_drain: 2s
`

func TestLoadFile_OK_Single(t *testing.T) {
	cfg, err := LoadFile(writeYAML(t, singleYAML))
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:3307", cfg.Listen)
	require.Len(t, cfg.Users, 1)
	require.Equal(t, constant.ModeSingle, cfg.Backend.Mode)
	require.Len(t, cfg.Backend.Slices, 1)
	require.Equal(t, "default", cfg.Backend.Slices[0].Name)
	require.Equal(t, "127.0.0.1:3306", cfg.Backend.Slices[0].Master.Address)
	require.Equal(t, "app", cfg.Backend.Slices[0].Master.User)
	require.Equal(t, "appdb", cfg.Backend.Slices[0].Master.Database)
	require.Equal(t, 2, cfg.Pool.Capacity)
	require.Equal(t, 8, cfg.Pool.MaxCapacity)
	require.Equal(t, 6, cfg.Pool.MaxIdle)
	require.Equal(t, 5*time.Minute, cfg.Pool.IdleTimeout)
	require.Equal(t, time.Hour, cfg.Pool.MaxLifetime)
	require.Equal(t, 30*time.Second, cfg.Pool.HealthCheckInterval)
	require.Equal(t, 3*time.Second, cfg.Pool.AcquireTimeout)
	require.Equal(t, 4*time.Second, cfg.Pool.DialTimeout)
	require.Equal(t, 15*time.Second, cfg.Pool.ReadTimeout)
	require.Equal(t, 16*time.Second, cfg.Pool.WriteTimeout)
	require.Equal(t, 20*time.Second, cfg.Server.ReadTimeout)
	require.Equal(t, 21*time.Second, cfg.Server.WriteTimeout)
	require.Equal(t, 8388608, cfg.Server.MaxAllowedPacket)
	require.Equal(t, "info", cfg.Log.Level)
	require.Equal(t, 2*time.Second, cfg.TerminationDrain)
}

func TestLoadFile_ModeDefaultSingle(t *testing.T) {
	cfg, err := LoadFile(writeYAML(t, `
listen: "127.0.0.1:3307"
users:
  - name: app
    password: secret
backend:
  slices:
    - name: default
      master:
        address: "127.0.0.1:3306"
        user: app
        password: secret
`))
	require.NoError(t, err)
	require.Equal(t, constant.ModeSingle, cfg.Backend.Mode)
}

func TestLoadFile_OK_RWSplit(t *testing.T) {
	cfg, err := LoadFile(writeYAML(t, `
listen: "127.0.0.1:3307"
users:
  - name: app
    password: secret
backend:
  mode: rwsplit
  slices:
    - name: default
      master:
        address: "127.0.0.1:3306"
        user: app
        password: secret
      slaves:
        - address: "127.0.0.1:3307"
          user: app
          password: secret
`))
	require.NoError(t, err)
	require.Equal(t, constant.ModeRWSplit, cfg.Backend.Mode)
	require.Len(t, cfg.Backend.Slices, 1)
	require.Len(t, cfg.Backend.Slices[0].Slaves, 1)
	require.Equal(t, constant.LoadBalanceRoundRobin, cfg.Backend.LoadBalance)
}

func TestLoadFile_RejectOldBackendServerKey(t *testing.T) {
	_, err := LoadFile(writeYAML(t, `
listen: "127.0.0.1:3307"
users:
  - name: app
    password: secret
backend_server:
  mode: single
  address: "127.0.0.1:3306"
  user: app
  password: secret
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "backend_server")
}

func TestLoadFile_RejectFlattenedBackend(t *testing.T) {
	_, err := LoadFile(writeYAML(t, `
listen: "127.0.0.1:3307"
users:
  - name: app
    password: secret
backend:
  address: "127.0.0.1:3306"
  user: app
  password: secret
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "address")
}

func TestLoadFile_RejectUnknownField(t *testing.T) {
	_, err := LoadFile(writeYAML(t, singleYAML+"\nnot_a_real_field: true\n"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not_a_real_field")
}

func TestValidate_RejectShardingAndUnknownMode(t *testing.T) {
	cfg := validSingle()
	cfg.Backend.Mode = constant.ModeSharding
	require.ErrorContains(t, Validate(cfg), "not implemented")

	cfg = validSingle()
	cfg.Backend.Mode = "weird"
	require.ErrorContains(t, Validate(cfg), "unsupported")

	cfg = validSingle()
	cfg.Server.MaxAllowedPacket = -1
	require.ErrorContains(t, Validate(cfg), "max_allowed_packet")
}

func TestValidate_SingleConstraints(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "no_slices", mutate: func(c *Config) { c.Backend.Slices = nil }, want: "exactly 1 slice"},
		{name: "two_slices", mutate: func(c *Config) {
			c.Backend.Slices = append(c.Backend.Slices, c.Backend.Slices[0])
		}, want: "duplicated"},
		{name: "slaves", mutate: func(c *Config) {
			c.Backend.Slices[0].Slaves = []BackendEndpoint{{Address: "x", User: "u"}}
		}, want: "must not set slaves"},
		{name: "sharding", mutate: func(c *Config) {
			c.Backend.Sharding = &ShardingConfig{}
		}, want: "must not set sharding"},
		{name: "load_balance", mutate: func(c *Config) {
			c.Backend.LoadBalance = constant.LoadBalanceRoundRobin
		}, want: "must not set load_balance"},
		{name: "empty_name", mutate: func(c *Config) {
			c.Backend.Slices[0].Name = ""
		}, want: "name is required"},
		{name: "missing_master_address", mutate: func(c *Config) {
			c.Backend.Slices[0].Master.Address = ""
		}, want: "master.address is required"},
		{name: "missing_master_user", mutate: func(c *Config) {
			c.Backend.Slices[0].Master.User = ""
		}, want: "master.user is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validSingle()
			tc.mutate(cfg)
			require.ErrorContains(t, Validate(cfg), tc.want)
		})
	}
}

func TestValidate_RWSplitConstraints(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "no_slices", mutate: func(c *Config) { c.Backend.Slices = nil }, want: "exactly 1 slice"},
		{name: "two_slices", mutate: func(c *Config) {
			second := c.Backend.Slices[0]
			second.Name = "other"
			c.Backend.Slices = append(c.Backend.Slices, second)
		}, want: "exactly 1 slice"},
		{name: "no_slaves", mutate: func(c *Config) {
			c.Backend.Slices[0].Slaves = nil
		}, want: "at least 1 slave"},
		{name: "bad_slave", mutate: func(c *Config) {
			c.Backend.Slices[0].Slaves[0].Address = ""
		}, want: "slaves[0].address is required"},
		{name: "bad_load_balance", mutate: func(c *Config) {
			c.Backend.LoadBalance = "nearest"
		}, want: "unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validRWSplit()
			tc.mutate(cfg)
			require.ErrorContains(t, Validate(cfg), tc.want)
		})
	}
}

func TestValidate_RequiresListenUsers(t *testing.T) {
	require.ErrorContains(t, Validate(&Config{}), "listen")

	cfg := validSingle()
	cfg.Users = nil
	require.ErrorContains(t, Validate(cfg), "users is required")

	cfg = validSingle()
	cfg.Users[0].Name = ""
	require.ErrorContains(t, Validate(cfg), "users[0].name")
}

func TestValidate_DuplicateSliceName(t *testing.T) {
	cfg := validSingle()
	second := cfg.Backend.Slices[0]
	cfg.Backend.Slices = append(cfg.Backend.Slices, second)
	require.ErrorContains(t, Validate(cfg), "duplicated")
}

func TestSameTopology(t *testing.T) {
	base := validRWSplit()
	require.NoError(t, Validate(base))

	same := base.Clone()
	require.True(t, base.Backend.SameTopology(same.Backend))

	modeChanged := base.Clone()
	modeChanged.Backend.Mode = constant.ModeSingle
	require.False(t, base.Backend.SameTopology(modeChanged.Backend))

	lbChanged := base.Clone()
	lbChanged.Backend.LoadBalance = constant.LoadBalanceRandom
	require.False(t, base.Backend.SameTopology(lbChanged.Backend))

	sliceChanged := base.Clone()
	sliceChanged.Backend.Slices[0].Master.Address = "10.0.0.1:3306"
	require.False(t, base.Backend.SameTopology(sliceChanged.Backend))

	shardingChanged := base.Clone()
	shardingChanged.Backend.Sharding = &ShardingConfig{Rules: []ShardRule{{Table: "t"}}}
	require.False(t, base.Backend.SameTopology(shardingChanged.Backend))
}

func TestClone_DeepCopy(t *testing.T) {
	base := validRWSplit()
	base.Backend.Sharding = &ShardingConfig{Rules: []ShardRule{{Table: "t", Slices: []string{"default"}}}}

	clone := base.Clone()
	clone.Users[0].Password = "changed"
	clone.Backend.Slices[0].Master.Address = "10.0.0.1:3306"
	clone.Backend.Slices[0].Slaves[0].Address = "10.0.0.2:3306"
	clone.Backend.Sharding.Rules[0].Slices[0] = "changed"

	require.Equal(t, "secret", base.Users[0].Password)
	require.Equal(t, "127.0.0.1:3306", base.Backend.Slices[0].Master.Address)
	require.Equal(t, "127.0.0.1:3307", base.Backend.Slices[0].Slaves[0].Address)
	require.Equal(t, "default", base.Backend.Sharding.Rules[0].Slices[0])
}

func TestRuntime_ReloadUsers(t *testing.T) {
	base := validSingle()
	base.Users = []User{{Name: "a", Password: "old"}}
	rt := NewRuntime(base)
	require.Equal(t, "old", rt.Load().Users[0].Password)

	next := base.Clone()
	next.Users = []User{{Name: "a", Password: "new"}}
	require.NoError(t, rt.Apply(next))
	require.Equal(t, "new", rt.Load().Users[0].Password)
}

func TestRuntime_RejectListenChange(t *testing.T) {
	base := validSingle()
	rt := NewRuntime(base)
	next := base.Clone()
	next.Listen = "127.0.0.1:3308"
	err := rt.Apply(next)
	require.ErrorContains(t, err, "listen")
	require.Equal(t, "127.0.0.1:3307", rt.Load().Listen)
}

func TestRuntime_RejectBackendChange(t *testing.T) {
	base := validSingle()
	rt := NewRuntime(base)
	next := base.Clone()
	next.Backend.Slices[0].Master.Address = "10.0.0.1:3306"
	err := rt.Apply(next)
	require.ErrorContains(t, err, "backend")
	require.Equal(t, "127.0.0.1:3306", rt.Load().Backend.Slices[0].Master.Address)
}

func TestRuntime_RejectPoolAndServerChange(t *testing.T) {
	base := validSingle()
	rt := NewRuntime(base)

	next := base.Clone()
	next.Pool.ReadTimeout = time.Second
	require.ErrorContains(t, rt.Apply(next), "pool")

	next = base.Clone()
	next.Server.MaxAllowedPacket = 1024
	require.ErrorContains(t, rt.Apply(next), "server")
}

func TestRuntime_ReloadFile(t *testing.T) {
	path := writeYAML(t, singleYAML)
	cfg, err := LoadFile(path)
	require.NoError(t, err)
	rt := NewRuntime(cfg)

	require.NoError(t, os.WriteFile(path, []byte(`
listen: "127.0.0.1:3307"
users:
  - name: app
    password: p2
server:
  read_timeout: 20s
  write_timeout: 21s
  max_allowed_packet: 8388608
pool:
  capacity: 2
  max_capacity: 8
  max_idle: 6
  idle_timeout: 5m
  max_lifetime: 1h
  health_check_interval: 30s
  acquire_timeout: 3s
  dial_timeout: 4s
  read_timeout: 15s
  write_timeout: 16s
backend:
  mode: single
  slices:
    - name: default
      master:
        address: "127.0.0.1:3306"
        user: app
        password: secret
        database: appdb
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
		Backend: BackendConfig{
			Mode: constant.ModeSingle,
			Slices: []SliceConfig{{
				Name: "default",
				Master: BackendEndpoint{
					Address:  "127.0.0.1:3306",
					User:     "app",
					Password: "secret",
					Database: "appdb",
				},
			}},
		},
		Log: Log{Level: "info"},
	}
}

func validRWSplit() *Config {
	cfg := validSingle()
	cfg.Backend = BackendConfig{
		Mode: constant.ModeRWSplit,
		Slices: []SliceConfig{{
			Name:   "default",
			Master: BackendEndpoint{Address: "127.0.0.1:3306", User: "app", Password: "secret"},
			Slaves: []BackendEndpoint{{Address: "127.0.0.1:3307", User: "app", Password: "secret"}},
		}},
	}
	return cfg
}

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prism.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}
