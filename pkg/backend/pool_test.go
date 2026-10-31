package backend_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acorndb/prism/pkg/backend"
	"github.com/acorndb/prism/pkg/result"
	"github.com/stretchr/testify/require"
)

type stubConn struct {
	id         int
	resetCalls atomic.Int32
	closeCalls atomic.Int32
	resetErr   error
	closed     atomic.Bool
}

func (s *stubConn) Reset(context.Context) error {
	s.resetCalls.Add(1)
	return s.resetErr
}

func (s *stubConn) Exec(context.Context, backend.ExecRequest) (result.Stream, error) {
	return nil, nil
}

func (s *stubConn) Align(context.Context, backend.AlignSettings) error { return nil }

func (s *stubConn) Close() error {
	s.closeCalls.Add(1)
	s.closed.Store(true)
	return nil
}

func TestPool_AcquireReleaseReuse(t *testing.T) {
	var n atomic.Int32
	p := backend.NewPool(backend.PoolConfig{Capacity: 1, MaxCapacity: 2, GetTimeout: time.Second}, func(ctx context.Context) (backend.Conn, error) {
		id := int(n.Add(1))
		return &stubConn{id: id}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	ctx := context.Background()
	b1, err := p.Acquire(ctx)
	require.NoError(t, err)
	s1 := b1.(*stubConn)
	require.Equal(t, 1, s1.id)

	require.NoError(t, p.Release(b1))
	require.Equal(t, int32(1), s1.resetCalls.Load())

	b2, err := p.Acquire(ctx)
	require.NoError(t, err)
	require.Same(t, b1, b2, "should reuse idle backend")
	require.Equal(t, int32(1), n.Load(), "no new open")
}

func TestPool_ReleaseResetFailureDiscards(t *testing.T) {
	var n atomic.Int32
	p := backend.NewPool(backend.PoolConfig{Capacity: 1, MaxCapacity: 2, GetTimeout: time.Second}, func(ctx context.Context) (backend.Conn, error) {
		id := int(n.Add(1))
		return &stubConn{id: id, resetErr: errors.New("reset failed")}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	ctx := context.Background()
	b1, err := p.Acquire(ctx)
	require.NoError(t, err)
	s1 := b1.(*stubConn)
	require.Error(t, p.Release(b1), "reset failure is surfaced")
	require.Equal(t, int32(1), s1.closeCalls.Load())

	b2, err := p.Acquire(ctx)
	require.NoError(t, err)
	require.NotSame(t, b1, b2)
	require.Equal(t, int32(2), n.Load())
}

func TestPool_DiscardFreesCapacity(t *testing.T) {
	var n atomic.Int32
	p := backend.NewPool(backend.PoolConfig{Capacity: 0, MaxCapacity: 1, GetTimeout: 50 * time.Millisecond}, func(ctx context.Context) (backend.Conn, error) {
		return &stubConn{id: int(n.Add(1))}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	ctx := context.Background()
	b1, err := p.Acquire(ctx)
	require.NoError(t, err)
	require.NoError(t, p.Discard(b1))
	require.Equal(t, int32(1), b1.(*stubConn).closeCalls.Load())

	b2, err := p.Acquire(ctx)
	require.NoError(t, err)
	require.NotSame(t, b1, b2)
	require.Equal(t, int32(2), n.Load())
}

func TestPool_MaxCapacity(t *testing.T) {
	var n atomic.Int32
	p := backend.NewPool(backend.PoolConfig{Capacity: 0, MaxCapacity: 1, GetTimeout: 50 * time.Millisecond}, func(ctx context.Context) (backend.Conn, error) {
		return &stubConn{id: int(n.Add(1))}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	ctx := context.Background()
	b1, err := p.Acquire(ctx)
	require.NoError(t, err)

	_, err = p.Acquire(ctx)
	require.Error(t, err, "second acquire should time out at max capacity")

	require.NoError(t, p.Release(b1))
	b2, err := p.Acquire(ctx)
	require.NoError(t, err)
	require.Same(t, b1, b2)
}
