// Package constant holds process-wide string enums (YAML values, mode names).
// Keep this package dependency-free so config/router/executor can share it.
package constant

// backend.mode
const (
	ModeSingle   = "single"
	ModeRWSplit  = "rwsplit"
	ModeSharding = "sharding"
)

// backend.load_balance
const (
	LoadBalanceRoundRobin = "round_robin"
	LoadBalanceRandom     = "random"
)

// Role is the backend role selected for one target.
type Role string

const (
	RoleMaster Role = "master"
	RoleSlave  Role = "slave"
)

// log.level
const (
	LogLevelDebug   = "debug"
	LogLevelInfo    = "info"
	LogLevelWarn    = "warn"
	LogLevelWarning = "warning"
	LogLevelError   = "error"
)
