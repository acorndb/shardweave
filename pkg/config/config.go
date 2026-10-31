// Package config loads and validates prism YAML configuration.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/acorndb/prism/pkg/constant"
	"go.yaml.in/yaml/v3"
)

// Config is the process configuration snapshot.
type Config struct {
	Listen           string        `yaml:"listen"`
	Users            []User        `yaml:"users"`
	BackendServer    BackendServer `yaml:"backend_server"`
	Pool             Pool          `yaml:"pool"`
	Log              Log           `yaml:"log"`
	TerminationDrain time.Duration `yaml:"termination_drain"`
}

// User is a proxy frontend credential.
type User struct {
	Name     string `yaml:"name"`
	Password string `yaml:"password"`
}

// BackendServer is the top-level backend topology (single / rwsplit / sharding).
type BackendServer struct {
	Mode string `yaml:"mode"`

	// single: endpoint fields inline
	Address  string `yaml:"address,omitempty"`
	User     string `yaml:"user,omitempty"`
	Password string `yaml:"password,omitempty"`
	Database string `yaml:"database,omitempty"`

	// rwsplit
	Master      *BackendEndpoint  `yaml:"master,omitempty"`
	Slaves      []BackendEndpoint `yaml:"slaves,omitempty"`
	LoadBalance string            `yaml:"load_balance,omitempty"`
}

// BackendEndpoint is one MySQL address (one pool leaf).
type BackendEndpoint struct {
	Address  string `yaml:"address"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Database string `yaml:"database"`
}

// Pool maps to backend_server.Config fields.
type Pool struct {
	Capacity    int           `yaml:"capacity"`
	MaxCapacity int           `yaml:"max_capacity"`
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	GetTimeout  time.Duration `yaml:"get_timeout"`
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

// Validate checks required fields for backend_server modes.
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
	for i, u := range cfg.Users {
		if u.Name == "" {
			return fmt.Errorf("config: users[%d].name is required", i)
		}
	}
	return validateBackendServer(&cfg.BackendServer)
}

func validateBackendServer(bs *BackendServer) error {
	mode := strings.TrimSpace(bs.Mode)
	if mode == "" {
		mode = constant.ModeSingle
		bs.Mode = constant.ModeSingle
	}
	switch mode {
	case constant.ModeSingle:
		if bs.Master != nil || len(bs.Slaves) > 0 {
			return fmt.Errorf("config: backend_server.mode=single must not set master/slaves")
		}
		if bs.Address == "" {
			return fmt.Errorf("config: backend_server.address is required")
		}
		if bs.User == "" {
			return fmt.Errorf("config: backend_server.user is required")
		}
		return nil
	case constant.ModeRWSplit:
		if bs.Address != "" || bs.User != "" || bs.Password != "" || bs.Database != "" {
			return fmt.Errorf("config: backend_server.mode=rwsplit must use master/slaves, not top-level address")
		}
		if bs.Master == nil {
			return fmt.Errorf("config: backend_server.master is required for rwsplit")
		}
		if err := validateEndpoint("backend_server.master", bs.Master); err != nil {
			return err
		}
		for i := range bs.Slaves {
			if err := validateEndpoint(fmt.Sprintf("backend_server.slaves[%d]", i), &bs.Slaves[i]); err != nil {
				return err
			}
		}
		switch bs.LoadBalance {
		case "", constant.LoadBalanceRoundRobin, constant.LoadBalanceRandom:
		default:
			return fmt.Errorf("config: backend_server.load_balance %q unsupported", bs.LoadBalance)
		}
		return nil
	case constant.ModeSharding:
		return fmt.Errorf("config: backend_server.mode=sharding is not implemented")
	default:
		return fmt.Errorf("config: backend_server.mode %q unsupported (want single|rwsplit|sharding)", mode)
	}
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

// Primary returns the writable/primary endpoint for single or rwsplit.
func (bs BackendServer) Primary() (BackendEndpoint, error) {
	mode := bs.Mode
	if mode == "" {
		mode = constant.ModeSingle
	}
	switch mode {
	case constant.ModeSingle:
		return BackendEndpoint{
			Address:  bs.Address,
			User:     bs.User,
			Password: bs.Password,
			Database: bs.Database,
		}, nil
	case constant.ModeRWSplit:
		if bs.Master == nil {
			return BackendEndpoint{}, fmt.Errorf("config: rwsplit master missing")
		}
		return *bs.Master, nil
	default:
		return BackendEndpoint{}, fmt.Errorf("config: primary unsupported for mode %q", mode)
	}
}

// SameIdentity reports whether topology endpoints/credentials are unchanged (reload gate).
func (bs BackendServer) SameIdentity(other BackendServer) bool {
	if strings.TrimSpace(bs.Mode) == "" {
		bs.Mode = constant.ModeSingle
	}
	if strings.TrimSpace(other.Mode) == "" {
		other.Mode = constant.ModeSingle
	}
	if bs.Mode != other.Mode || bs.LoadBalance != other.LoadBalance {
		return false
	}
	a, errA := bs.Primary()
	b, errB := other.Primary()
	if errA != nil || errB != nil {
		return false
	}
	if a != b {
		return false
	}
	if len(bs.Slaves) != len(other.Slaves) {
		return false
	}
	for i := range bs.Slaves {
		if bs.Slaves[i] != other.Slaves[i] {
			return false
		}
	}
	return true
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
	out.BackendServer = c.BackendServer.Clone()
	return &out
}

// Clone deep-copies BackendServer.
func (bs BackendServer) Clone() BackendServer {
	out := bs
	if bs.Master != nil {
		m := *bs.Master
		out.Master = &m
	}
	if bs.Slaves != nil {
		out.Slaves = append([]BackendEndpoint(nil), bs.Slaves...)
	}
	return out
}
