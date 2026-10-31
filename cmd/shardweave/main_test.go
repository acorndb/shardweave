package main

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/acorndb/shardweave/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestExampleConfigLoads(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	cfg, err := config.LoadFile(filepath.Join(root, "etc", "shardweave.yaml"))
	require.NoError(t, err)
	require.Equal(t, "0.0.0.0:3307", cfg.Listen)
	require.NotEmpty(t, cfg.Users)
	require.NotEmpty(t, cfg.Backend.Slices)
	require.NotEmpty(t, cfg.Backend.Slices[0].Master.Address)
}
