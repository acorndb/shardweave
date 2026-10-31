// Package config loads and validates prism YAML configuration.
package config

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/acorndb/prism/pkg/constant"
	"go.yaml.in/yaml/v3"
)

// Config is the process configuration snapshot.
type Config struct {
	Listen           string        `yaml:"listen"`
	Users            []User        `yaml:"users"`
	Server           Server        `yaml:"server"`
	Backend          BackendConfig `yaml:"backend"`
	Pool             Pool          `yaml:"pool"`
	Log              Log           `yaml:"log"`
	TerminationDrain time.Duration `yaml:"termination_drain"`
}

// Server holds frontend I/O limits and instance-wide packet limit.
type Server struct {
	ReadTimeout      time.Duration `yaml:"read_timeout"`
	WriteTimeout     time.Duration `yaml:"write_timeout"`
	MaxAllowedPacket int           `yaml:"max_allowed_packet"` // bytes; 0 = 64 MiB
	MaxConnections   int           `yaml:"max_connections"`    // 0 = unlimited
}

// User is a proxy frontend credential.
type User struct {
	Name     string `yaml:"name"`
	Password string `yaml:"password"`
}

// BackendConfig is the backend topology: slices + mode-specific constraints.
type BackendConfig struct {
	Mode        string          `yaml:"mode"`
	Slices      []SliceConfig   `yaml:"slices"`
	LoadBalance string          `yaml:"load_balance,omitempty"`
	Sharding    *ShardingConfig `yaml:"sharding,omitempty"`
}

// SliceConfig is one logical topology unit: master + optional slaves.
type SliceConfig struct {
	Name   string            `yaml:"name"`
	Master BackendEndpoint   `yaml:"master"`
	Slaves []BackendEndpoint `yaml:"slaves,omitempty"`
}

// BackendEndpoint is one MySQL address (one pool leaf).
type BackendEndpoint struct {
	Address  string `yaml:"address"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Database string `yaml:"database"`
}

// ShardingConfig describes table sharding rules. Parsed now, rejected until P3.
type ShardingConfig struct {
	Rules []ShardRule `yaml:"rules"`
}

// ShardRule maps one logical table to slices.
type ShardRule struct {
	Table     string   `yaml:"table"`
	Key       string   `yaml:"key"`
	Algorithm string   `yaml:"algorithm"`
	Slices    []string `yaml:"slices"`
}

// Pool maps to backend.PoolConfig fields.
type Pool struct {
	Capacity            int           `yaml:"capacity"`
	MaxCapacity         int           `yaml:"max_capacity"`
	MaxIdle             int           `yaml:"max_idle"`
	IdleTimeout         time.Duration `yaml:"idle_timeout"`
	MaxLifetime         time.Duration `yaml:"max_lifetime"`
	HealthCheckInterval time.Duration `yaml:"health_check_interval"`
	AcquireTimeout      time.Duration `yaml:"acquire_timeout"`
	DialTimeout         time.Duration `yaml:"dial_timeout"`
	ReadTimeout         time.Duration `yaml:"read_timeout"`
	WriteTimeout        time.Duration `yaml:"write_timeout"`
}

// Log holds process logging settings.
type Log struct {
	Level string `yaml:"level"`
}

// LoadFile reads and validates a YAML config file.
func LoadFile(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: decode %s: %w", path, err)
	}
	if err := Validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks required fields and backend mode constraints.
func Validate(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config: nil")
	}
	if cfg.Listen == "" {
		return fmt.Errorf("config: listen is required")
	}
	if len(cfg.Users) == 0 {
		return fmt.Errorf("config: users is required")
	}
	if cfg.Server.MaxAllowedPacket < 0 {
		return fmt.Errorf("config: server.max_allowed_packet must be >= 0")
	}
	if cfg.Server.MaxConnections < 0 {
		return fmt.Errorf("config: server.max_connections must be >= 0")
	}
	for i, u := range cfg.Users {
		if u.Name == "" {
			return fmt.Errorf("config: users[%d].name is required", i)
		}
	}
	return validateBackend(&cfg.Backend)
}

func validateBackend(b *BackendConfig) error {
	mode := strings.TrimSpace(b.Mode)
	if mode == "" {
		mode = constant.ModeSingle
		b.Mode = constant.ModeSingle
	}

	seen := make(map[string]bool, len(b.Slices))
	for i := range b.Slices {
		name := b.Slices[i].Name
		if name == "" {
			continue // reported by validateSlice in supported modes
		}
		if seen[name] {
			return fmt.Errorf("config: backend.slices[%d].name %q is duplicated", i, name)
		}
		seen[name] = true
	}

	switch mode {
	case constant.ModeSingle:
		if len(b.Slices) != 1 {
			return fmt.Errorf("config: backend.mode=single requires exactly 1 slice")
		}
		if len(b.Slices[0].Slaves) > 0 {
			return fmt.Errorf("config: backend.mode=single must not set slaves")
		}
		if b.Sharding != nil {
			return fmt.Errorf("config: backend.mode=single must not set sharding")
		}
		if b.LoadBalance != "" {
			return fmt.Errorf("config: backend.mode=single must not set load_balance")
		}
		return validateSlice("backend.slices[0]", &b.Slices[0])
	case constant.ModeRWSplit:
		if len(b.Slices) != 1 {
			return fmt.Errorf("config: backend.mode=rwsplit requires exactly 1 slice")
		}
		if b.Sharding != nil {
			return fmt.Errorf("config: backend.mode=rwsplit must not set sharding")
		}
		if len(b.Slices[0].Slaves) == 0 {
			return fmt.Errorf("config: backend.mode=rwsplit requires at least 1 slave")
		}
		if err := validateSlice("backend.slices[0]", &b.Slices[0]); err != nil {
			return err
		}
		switch b.LoadBalance {
		case "":
			b.LoadBalance = constant.LoadBalanceRoundRobin
		case constant.LoadBalanceRoundRobin, constant.LoadBalanceRandom:
		default:
			return fmt.Errorf("config: backend.load_balance %q unsupported", b.LoadBalance)
		}
		return nil
	case constant.ModeSharding:
		return fmt.Errorf("config: backend.mode=sharding is not implemented")
	default:
		return fmt.Errorf("config: backend.mode %q unsupported (want single|rwsplit|sharding)", mode)
	}
}

func validateSlice(prefix string, s *SliceConfig) error {
	if s.Name == "" {
		return fmt.Errorf("config: %s.name is required", prefix)
	}
	if err := validateEndpoint(prefix+".master", &s.Master); err != nil {
		return err
	}
	for i := range s.Slaves {
		if err := validateEndpoint(fmt.Sprintf("%s.slaves[%d]", prefix, i), &s.Slaves[i]); err != nil {
			return err
		}
	}
	return nil
}

func validateEndpoint(prefix string, ep *BackendEndpoint) error {
	if ep.Address == "" {
		return fmt.Errorf("config: %s.address is required", prefix)
	}
	if ep.User == "" {
		return fmt.Errorf("config: %s.user is required", prefix)
	}
	return nil
}

func normalizeMode(mode string) string {
	if strings.TrimSpace(mode) == "" {
		return constant.ModeSingle
	}
	return mode
}

// SameTopology reports whether mode/slices/rules/LB are unchanged (reload gate).
func (b BackendConfig) SameTopology(other BackendConfig) bool {
	if normalizeMode(b.Mode) != normalizeMode(other.Mode) {
		return false
	}
	if b.LoadBalance != other.LoadBalance {
		return false
	}
	if !reflect.DeepEqual(b.Slices, other.Slices) {
		return false
	}
	return reflect.DeepEqual(b.Sharding, other.Sharding)
}

// Clone returns a deep copy suitable for mutation before Apply.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	out := *c
	if c.Users != nil {
		out.Users = append([]User(nil), c.Users...)
	}
	out.Backend = c.Backend.Clone()
	return &out
}

// Clone deep-copies BackendConfig.
func (b BackendConfig) Clone() BackendConfig {
	out := b
	if b.Slices != nil {
		out.Slices = make([]SliceConfig, len(b.Slices))
		for i := range b.Slices {
			out.Slices[i] = b.Slices[i]
			if b.Slices[i].Slaves != nil {
				out.Slices[i].Slaves = append([]BackendEndpoint(nil), b.Slices[i].Slaves...)
			}
		}
	}
	if b.Sharding != nil {
		sc := *b.Sharding
		if b.Sharding.Rules != nil {
			sc.Rules = make([]ShardRule, len(b.Sharding.Rules))
			for i := range b.Sharding.Rules {
				sc.Rules[i] = b.Sharding.Rules[i]
				if b.Sharding.Rules[i].Slices != nil {
					sc.Rules[i].Slices = append([]string(nil), b.Sharding.Rules[i].Slices...)
				}
			}
		}
		out.Sharding = &sc
	}
	return out
}
