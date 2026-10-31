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

// sharding.algorithm
const (
	// ShardingHashMod hashes integer shard keys: uint64(value) % len(slices).
	// String literals are accepted only when they are valid decimal integers.
	ShardingHashMod = "hash_mod"
	// ShardingHashModStr hashes string shard keys: FNV-1a 64 % len(slices).
	// Non-string literals are rejected so that quoted/typed forms cannot drift.
	ShardingHashModStr = "hash_mod_str"
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
