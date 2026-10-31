package harness

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/stretchr/testify/require"
)

func TestShardedProxyConfig(t *testing.T) {
	tp := &ShardedTopology{Slices: []*Instance{
		{Name: "s0", addr: "127.0.0.1:33061"},
		{Name: "s1", addr: "127.0.0.1:33062"},
	}}
	cfg := tp.ProxyConfig()

	require.Equal(t, constant.ModeSharding, cfg.Backend.Mode)
	require.Len(t, cfg.Backend.Slices, 2)
	require.Len(t, cfg.Backend.Sharding.Rules, 1)
	rule := cfg.Backend.Sharding.Rules[0]
	require.Equal(t, "orders", rule.Table)
	require.Equal(t, "user_id", rule.Key)
	require.Equal(t, constant.ShardingHashMod, rule.Algorithm)
	require.Equal(t, []string{"s0", "s1"}, rule.Slices)
}
