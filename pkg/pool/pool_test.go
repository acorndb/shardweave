package pool_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acorndb/prism/pkg/pool"
	"github.com/stretchr/testify/require"
)

type stubBackend struct {
	id          int
	resetCalls  atomic.Int32
	closeCalls  atomic.Int32
	resetErr    error
	closed      atomic.Bool
}

func (s *stubBackend) Reset(context.Context) error {
	s.resetCalls.Add(1)
	return s.resetErr
}

func (s *stubBackend) Close() error {
	s.closeCalls.Add(1)
	s.closed.Store(true)
	return nil
}

func TestPool_AcquireReleaseReuse(t *testing.T) {
	var n atomic.Int32
	p := pool.New(pool.Config{Capacity: 1, MaxCapacity: 2, GetTimeout: time.Second}, func(ctx context.Context) (pool.Backend, error) {
		id := int(n.Add(1))
		return &stubBackend{id: id}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	ctx := context.Background()
	b1, err := p.Acquire(ctx)
	require.NoError(t, err)
	s1 := b1.(*stubBackend)
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
	p := pool.New(pool.Config{Capacity: 1, MaxCapacity: 2, GetTimeout: time.Second}, func(ctx context.Context) (pool.Backend, error) {
		id := int(n.Add(1))
		return &stubBackend{id: id, resetErr: errors.New("reset failed")}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	ctx := context.Background()
	b1, err := p.Acquire(ctx)
	require.NoError(t, err)
	s1 := b1.(*stubBackend)
	require.Error(t, p.Release(b1), "reset failure is surfaced")
	require.Equal(t, int32(1), s1.closeCalls.Load())

	b2, err := p.Acquire(ctx)
	require.NoError(t, err)
	require.NotSame(t, b1, b2)
	require.Equal(t, int32(2), n.Load())
}

func TestPool_DiscardFreesCapacity(t *testing.T) {
	var n atomic.Int32
	p := pool.New(pool.Config{Capacity: 0, MaxCapacity: 1, GetTimeout: 50 * time.Millisecond}, func(ctx context.Context) (pool.Backend, error) {
		return &stubBackend{id: int(n.Add(1))}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	ctx := context.Background()
	b1, err := p.Acquire(ctx)
	require.NoError(t, err)
	require.NoError(t, p.Discard(b1))
	require.Equal(t, int32(1), b1.(*stubBackend).closeCalls.Load())

	b2, err := p.Acquire(ctx)
	require.NoError(t, err)
	require.NotSame(t, b1, b2)
	require.Equal(t, int32(2), n.Load())
}

func TestPool_MaxCapacity(t *testing.T) {
	var n atomic.Int32
	p := pool.New(pool.Config{Capacity: 0, MaxCapacity: 1, GetTimeout: 50 * time.Millisecond}, func(ctx context.Context) (pool.Backend, error) {
		return &stubBackend{id: int(n.Add(1))}, nil
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
