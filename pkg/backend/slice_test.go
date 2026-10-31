package backend_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acorndb/prism/pkg/backend"
	"github.com/acorndb/prism/pkg/constant"
	"github.com/acorndb/prism/pkg/result"
	"github.com/stretchr/testify/require"
)

type sliceStubConn struct {
	id int
}

func (s *sliceStubConn) Ping(context.Context) error  { return nil }
func (s *sliceStubConn) Reset(context.Context) error { return nil }
func (s *sliceStubConn) Close() error                { return nil }
func (s *sliceStubConn) Exec(context.Context, backend.ExecRequest) (result.Stream, error) {
	return nil, nil
}
func (s *sliceStubConn) Align(context.Context, backend.AlignSettings) error { return nil }

func newPool(t *testing.T, idBase int, fail bool) *backend.Pool {
	t.Helper()
	var n atomic.Int32
	return backend.NewPool(backend.PoolConfig{Capacity: 0, MaxCapacity: 4, GetTimeout: time.Second}, func(ctx context.Context) (backend.Conn, error) {
		if fail {
			return nil, errors.New("dial fail")
		}
		return &sliceStubConn{id: idBase + int(n.Add(1))}, nil
	})
}

func TestSlice_AcquireMaster(t *testing.T) {
	g := &backend.Slice{Master: newPool(t, 100, false)}
	conn, p, err := g.Acquire(context.Background(), constant.RoleMaster)
	require.NoError(t, err)
	require.Same(t, g.Master, p)
	require.NotNil(t, conn)
	require.NoError(t, g.Release(p, conn))
}

func TestSlice_AcquireSlaveRoundRobin(t *testing.T) {
	s0 := newPool(t, 0, false)
	s1 := newPool(t, 10, false)
	g := &backend.Slice{
		Master:      newPool(t, 100, false),
		Slaves:      []*backend.Pool{s0, s1},
		LoadBalance: constant.LoadBalanceRoundRobin,
	}
	c1, p1, err := g.Acquire(context.Background(), constant.RoleSlave)
	require.NoError(t, err)
	c2, p2, err := g.Acquire(context.Background(), constant.RoleSlave)
	require.NoError(t, err)
	require.True(t, (p1 == s0 && p2 == s1) || (p1 == s1 && p2 == s0) || p1 != p2,
		"expected round-robin across slaves, got %v %v", p1 == s0, p2 == s0)
	_ = c1
	_ = c2
	require.NoError(t, g.Release(p1, c1))
	require.NoError(t, g.Release(p2, c2))
}

func TestSlice_SlaveFailFallsBackMaster(t *testing.T) {
	g := &backend.Slice{
		Master: newPool(t, 100, false),
		Slaves: []*backend.Pool{newPool(t, 0, true)},
	}
	conn, p, err := g.Acquire(context.Background(), constant.RoleSlave)
	require.NoError(t, err)
	require.Same(t, g.Master, p)
	require.NoError(t, g.Release(p, conn))
}

func TestSlice_NoSlavesUsesMaster(t *testing.T) {
	g := &backend.Slice{Master: newPool(t, 100, false)}
	conn, p, err := g.Acquire(context.Background(), constant.RoleSlave)
	require.NoError(t, err)
	require.Same(t, g.Master, p)
	require.NoError(t, g.Release(p, conn))
}

func TestSlice_NilMasterErrors(t *testing.T) {
	g := &backend.Slice{}
	_, _, err := g.Acquire(context.Background(), constant.RoleMaster)
	require.Error(t, err)
}
