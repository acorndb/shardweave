package backend_test

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/backend"
	"github.com/stretchr/testify/require"
)

func TestNewCluster(t *testing.T) {
	_, err := backend.NewCluster()
	require.Error(t, err)

	_, err = backend.NewCluster(nil)
	require.Error(t, err)

	_, err = backend.NewCluster(&backend.Slice{})
	require.Error(t, err)

	_, err = backend.NewCluster(&backend.Slice{Name: "s0"}, &backend.Slice{Name: "s0"})
	require.Error(t, err)

	c, err := backend.NewCluster(&backend.Slice{Name: "s0"}, &backend.Slice{Name: "s1"})
	require.NoError(t, err)
	require.Equal(t, []string{"s0", "s1"}, c.Names())
	require.NoError(t, c.Close())
}

func TestCluster_Get(t *testing.T) {
	s0 := &backend.Slice{Name: "s0"}
	c, err := backend.NewCluster(s0)
	require.NoError(t, err)

	got, err := c.Get("s0")
	require.NoError(t, err)
	require.Same(t, s0, got)

	_, err = c.Get("missing")
	require.Error(t, err)

	var nilCluster *backend.Cluster
	_, err = nilCluster.Get("s0")
	require.Error(t, err)
	require.Nil(t, nilCluster.Names())
	require.NoError(t, nilCluster.Close())
}
