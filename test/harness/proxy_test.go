package harness

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/stretchr/testify/require"
)

func TestProxyConfig(t *testing.T) {
	tp := &Topology{
		Master: &Instance{Name: "master", ServerID: 1, addr: "127.0.0.1:33061"},
		Replicas: []*Instance{
			{Name: "replica-1", ServerID: 2, addr: "127.0.0.1:33062"},
			{Name: "replica-2", ServerID: 3, addr: "127.0.0.1:33063"},
		},
	}

	cfg := tp.ProxyConfig()

	require.Equal(t, "127.0.0.1:0", cfg.Listen)
	require.Len(t, cfg.Users, 1)
	require.Equal(t, AppUser, cfg.Users[0].Name)
	require.Equal(t, AppPassword, cfg.Users[0].Password)
	require.Equal(t, constant.ModeRWSplit, cfg.Backend.Mode)
	require.Len(t, cfg.Backend.Slices, 1)

	slice := cfg.Backend.Slices[0]
	require.Equal(t, "default", slice.Name)
	require.Equal(t, "127.0.0.1:33061", slice.Master.Address)
	require.Equal(t, AppUser, slice.Master.User)
	require.Equal(t, Database, slice.Master.Database)
	require.Len(t, slice.Slaves, 2)
	require.Equal(t, "127.0.0.1:33062", slice.Slaves[0].Address)
	require.Equal(t, "127.0.0.1:33063", slice.Slaves[1].Address)
}
