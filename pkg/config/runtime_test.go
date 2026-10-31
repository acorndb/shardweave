package config

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/stretchr/testify/require"
)

func TestRuntime_ApplyRejectsModeChange(t *testing.T) {
	t.Run("single_to_rwsplit", func(t *testing.T) {
		base := validSingle()
		rt := NewRuntime(base)

		err := rt.Apply(validRWSplit())
		require.ErrorContains(t, err, "mode")
		require.Equal(t, constant.ModeSingle, rt.Load().Backend.Mode)
	})

	t.Run("sharding_to_single", func(t *testing.T) {
		base := validSharding()
		rt := NewRuntime(base)

		err := rt.Apply(validSingle())
		require.ErrorContains(t, err, "mode")
		require.Equal(t, constant.ModeSharding, rt.Load().Backend.Mode)
	})

	t.Run("rwsplit_to_sharding", func(t *testing.T) {
		base := validRWSplit()
		rt := NewRuntime(base)

		err := rt.Apply(validSharding())
		require.ErrorContains(t, err, "mode")
		require.Equal(t, constant.ModeRWSplit, rt.Load().Backend.Mode)
	})
}

func TestRuntime_ApplyAllowsNormalizedMode(t *testing.T) {
	base := validSingle()
	base.Backend.Mode = ""
	rt := NewRuntime(base)

	next := base.Clone()
	next.Backend.Mode = constant.ModeSingle
	require.NoError(t, rt.Apply(next))
}
