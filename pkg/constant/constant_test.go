package constant_test

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/stretchr/testify/require"
)

func TestModeValues(t *testing.T) {
	require.Equal(t, "single", constant.ModeSingle)
	require.Equal(t, "rwsplit", constant.ModeRWSplit)
	require.Equal(t, "sharding", constant.ModeSharding)
}

func TestLoadBalanceValues(t *testing.T) {
	require.Equal(t, "round_robin", constant.LoadBalanceRoundRobin)
	require.Equal(t, "random", constant.LoadBalanceRandom)
}
