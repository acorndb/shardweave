package router_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acorndb/prism/pkg/backend_server"
	"github.com/acorndb/prism/pkg/constant"
	"github.com/acorndb/prism/pkg/router"
	"github.com/stretchr/testify/require"
)

type stubBackend struct {
	id int
}

func (s *stubBackend) Reset(context.Context) error { return nil }
func (s *stubBackend) Close() error                { return nil }

func newPool(t *testing.T, idBase int, fail bool) *backend_server.Pool {
	t.Helper()
	var n atomic.Int32
	return backend_server.New(backend_server.Config{Capacity: 1, MaxCapacity: 4, GetTimeout: time.Second}, func(ctx context.Context) (backend_server.Backend, error) {
		if fail {
			return nil, errors.New("dial fail")
		}
		return &stubBackend{id: idBase + int(n.Add(1))}, nil
	})
}

func TestGroup_AcquireMaster(t *testing.T) {
	g := &router.Group{Master: newPool(t, 100, false)}
	conn, p, err := g.Acquire(context.Background(), router.RoleMaster)
	require.NoError(t, err)
	require.Same(t, g.Master, p)
	require.NotNil(t, conn)
	require.NoError(t, g.Release(p, conn))
}

func TestGroup_AcquireSlaveRoundRobin(t *testing.T) {
	s0 := newPool(t, 0, false)
	s1 := newPool(t, 10, false)
	g := &router.Group{
		Master:      newPool(t, 100, false),
		Slaves:      []*backend_server.Pool{s0, s1},
		LoadBalance: constant.LoadBalanceRoundRobin,
	}
	c1, p1, err := g.Acquire(context.Background(), router.RoleSlave)
	require.NoError(t, err)
	c2, p2, err := g.Acquire(context.Background(), router.RoleSlave)
	require.NoError(t, err)
	require.True(t, (p1 == s0 && p2 == s1) || (p1 == s1 && p2 == s0) || p1 != p2,
		"expected round-robin across slaves, got %v %v", p1 == s0, p2 == s0)
	_ = c1
	_ = c2
	require.NoError(t, g.Release(p1, c1))
	require.NoError(t, g.Release(p2, c2))
}

func TestGroup_SlaveFailFallsBackMaster(t *testing.T) {
	g := &router.Group{
		Master: newPool(t, 100, false),
		Slaves: []*backend_server.Pool{newPool(t, 0, true)},
	}
	conn, p, err := g.Acquire(context.Background(), router.RoleSlave)
	require.NoError(t, err)
	require.Same(t, g.Master, p)
	require.NoError(t, g.Release(p, conn))
}

func TestGroup_NoSlavesUsesMaster(t *testing.T) {
	g := &router.Group{Master: newPool(t, 100, false)}
	conn, p, err := g.Acquire(context.Background(), router.RoleSlave)
	require.NoError(t, err)
	require.Same(t, g.Master, p)
	require.NoError(t, g.Release(p, conn))
}

func TestGroup_NilMasterErrors(t *testing.T) {
	g := &router.Group{}
	_, _, err := g.Acquire(context.Background(), router.RoleMaster)
	require.Error(t, err)
}
