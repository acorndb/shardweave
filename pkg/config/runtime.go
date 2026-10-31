package config

import (
	"fmt"
	"sync/atomic"
)

// Runtime holds the active config snapshot for hot reload.
type Runtime struct {
	cur atomic.Pointer[Config]
}

// NewRuntime stores the initial validated config.
func NewRuntime(cfg *Config) *Runtime {
	rt := &Runtime{}
	rt.cur.Store(cfg.Clone())
	return rt
}

// Load returns the current immutable snapshot (do not mutate).
func (rt *Runtime) Load() *Config {
	return rt.cur.Load()
}

// ReloadFile loads path and Apply; on failure the previous snapshot is kept.
func (rt *Runtime) ReloadFile(path string) error {
	next, err := LoadFile(path)
	if err != nil {
		return err
	}
	return rt.Apply(next)
}

// Apply swaps in next after validating reload constraints.
// listen and backend topology changes are rejected (require restart).
func (rt *Runtime) Apply(next *Config) error {
	if err := Validate(next); err != nil {
		return err
	}
	cur := rt.cur.Load()
	if next.Listen != cur.Listen {
		return fmt.Errorf("config: listen change requires restart (%q -> %q)", cur.Listen, next.Listen)
	}
	if !next.Backend.SameTopology(cur.Backend) {
		return fmt.Errorf("config: backend change requires restart")
	}
	if next.Server != cur.Server {
		return fmt.Errorf("config: server settings change requires restart")
	}
	if next.Pool != cur.Pool {
		return fmt.Errorf("config: pool settings change requires restart")
	}
	rt.cur.Store(next.Clone())
	return nil
}

// UserProvider returns a live view of frontend users from the runtime snapshot.
func (rt *Runtime) UserProvider() *LiveUsers {
	return &LiveUsers{rt: rt}
}

// LiveUsers implements mysql/server.UserProvider against Runtime.
type LiveUsers struct {
	rt *Runtime
}

// Password looks up the current password for user.
func (u *LiveUsers) Password(user string) (string, bool) {
	cfg := u.rt.Load()
	for _, x := range cfg.Users {
		if x.Name == user {
			return x.Password, true
		}
	}
	return "", false
}
