// Package config loads and validates prism YAML configuration.
package config

import (
	"fmt"
	"os"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is the process configuration snapshot.
type Config struct {
	Listen           string        `yaml:"listen"`
	Users            []User        `yaml:"users"`
	Backend          Backend       `yaml:"backend"`
	Pool             Pool          `yaml:"pool"`
	Log              Log           `yaml:"log"`
	TerminationDrain time.Duration `yaml:"termination_drain"`
}

// User is a proxy frontend credential.
type User struct {
	Name     string `yaml:"name"`
	Password string `yaml:"password"`
}

// Backend is the single MySQL target for M1 passthrough.
type Backend struct {
	Address  string `yaml:"address"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Database string `yaml:"database"`
}

// Pool maps to pool.Config fields.
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

// Validate checks required fields for M1 passthrough.
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
	if cfg.Backend.Address == "" {
		return fmt.Errorf("config: backend.address is required")
	}
	if cfg.Backend.User == "" {
		return fmt.Errorf("config: backend.user is required")
	}
	return nil
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
	return &out
}
