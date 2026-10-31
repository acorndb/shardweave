package backend_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acorndb/shardweave/pkg/backend"
	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/result"
	"github.com/stretchr/testify/require"
)

type stubConn struct {
	id         int
	resetCalls atomic.Int32
	closeCalls atomic.Int32
	pingCalls  atomic.Int32
	resetErr   error
	pingErr    error
	closed     atomic.Bool
}

func (s *stubConn) Ping(context.Context) error {
	s.pingCalls.Add(1)
	return s.pingErr
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
	p := backend.NewPool(backend.PoolConfig{Capacity: 0, MaxCapacity: 2, AcquireTimeout: time.Second}, func(ctx context.Context) (backend.Conn, error) {
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
	p := backend.NewPool(backend.PoolConfig{Capacity: 0, MaxCapacity: 2, AcquireTimeout: time.Second}, func(ctx context.Context) (backend.Conn, error) {
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
	p := backend.NewPool(backend.PoolConfig{Capacity: 0, MaxCapacity: 0, AcquireTimeout: 50 * time.Millisecond}, func(ctx context.Context) (backend.Conn, error) {
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
	p := backend.NewPool(backend.PoolConfig{Capacity: 0, MaxCapacity: 1}, func(ctx context.Context) (backend.Conn, error) {
		return &stubConn{id: int(n.Add(1))}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	ctx := context.Background()
	b1, err := p.Acquire(ctx)
	require.NoError(t, err)

	_, err = p.Acquire(ctx)
	require.ErrorIs(t, err, errs.ErrPoolExhausted, "full pool must fail fast by default")

	require.NoError(t, p.Release(b1))
	b2, err := p.Acquire(ctx)
	require.NoError(t, err)
	require.Same(t, b1, b2)
}

func TestPool_AcquireTimeoutWaits(t *testing.T) {
	var n atomic.Int32
	p := backend.NewPool(backend.PoolConfig{
		MaxCapacity:    1,
		AcquireTimeout: time.Second,
	}, func(ctx context.Context) (backend.Conn, error) {
		return &stubConn{id: int(n.Add(1))}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	b1, err := p.Acquire(context.Background())
	require.NoError(t, err)
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = p.Release(b1)
	}()

	b2, err := p.Acquire(context.Background())
	require.NoError(t, err)
	require.Same(t, b1, b2)
	require.NoError(t, p.Release(b2))
}

func TestPool_AcquireTimeoutExpires(t *testing.T) {
	var n atomic.Int32
	p := backend.NewPool(backend.PoolConfig{
		MaxCapacity:    1,
		AcquireTimeout: 20 * time.Millisecond,
	}, func(ctx context.Context) (backend.Conn, error) {
		return &stubConn{id: int(n.Add(1))}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	b1, err := p.Acquire(context.Background())
	require.NoError(t, err)

	_, err = p.Acquire(context.Background())
	require.ErrorIs(t, err, errs.ErrPoolExhausted)
	require.NoError(t, p.Release(b1))
}

func TestPool_Warmup(t *testing.T) {
	var n atomic.Int32
	p := backend.NewPool(backend.PoolConfig{
		Capacity: 2, MaxCapacity: 4, AcquireTimeout: time.Second,
	}, func(context.Context) (backend.Conn, error) {
		return &stubConn{id: int(n.Add(1))}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	require.Eventually(t, func() bool { return n.Load() == 2 }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return p.Stats().Idle == 2 }, time.Second, time.Millisecond)

	b1, err := p.Acquire(context.Background())
	require.NoError(t, err)
	b2, err := p.Acquire(context.Background())
	require.NoError(t, err)
	require.Equal(t, int32(2), n.Load(), "warmup connections should be reused")
	require.NoError(t, p.Release(b1))
	require.NoError(t, p.Release(b2))
}

func TestPool_HealthCheckDiscardsDeadIdle(t *testing.T) {
	var n atomic.Int32
	p := backend.NewPool(backend.PoolConfig{
		MaxCapacity:         2,
		HealthCheckInterval: 5 * time.Millisecond,
		AcquireTimeout:      time.Second,
	}, func(context.Context) (backend.Conn, error) {
		return &stubConn{id: int(n.Add(1))}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	b1, err := p.Acquire(context.Background())
	require.NoError(t, err)
	s1 := b1.(*stubConn)
	require.NoError(t, p.Release(b1))
	time.Sleep(10 * time.Millisecond)
	s1.pingErr = errors.New("dead")

	b2, err := p.Acquire(context.Background())
	require.NoError(t, err)
	require.NotSame(t, b1, b2)
	require.Equal(t, int32(1), s1.pingCalls.Load())
	require.Equal(t, int32(1), s1.closeCalls.Load())
	require.Equal(t, int32(2), n.Load())
	require.NoError(t, p.Release(b2))
}

func TestPool_HealthCheckReusesLiveIdle(t *testing.T) {
	var n atomic.Int32
	p := backend.NewPool(backend.PoolConfig{
		MaxCapacity:         2,
		HealthCheckInterval: 5 * time.Millisecond,
		AcquireTimeout:      time.Second,
	}, func(context.Context) (backend.Conn, error) {
		return &stubConn{id: int(n.Add(1))}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	b1, err := p.Acquire(context.Background())
	require.NoError(t, err)
	s1 := b1.(*stubConn)
	require.NoError(t, p.Release(b1))
	time.Sleep(10 * time.Millisecond)

	b2, err := p.Acquire(context.Background())
	require.NoError(t, err)
	require.Same(t, b1, b2)
	require.Equal(t, int32(1), s1.pingCalls.Load())
	require.Equal(t, int32(1), n.Load())
	require.NoError(t, p.Release(b2))
}

func TestPool_IdleReap(t *testing.T) {
	var n atomic.Int32
	p := backend.NewPool(backend.PoolConfig{
		MaxCapacity:    2,
		IdleTimeout:    10 * time.Millisecond,
		AcquireTimeout: time.Second,
	}, func(context.Context) (backend.Conn, error) {
		return &stubConn{id: int(n.Add(1))}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	b1, err := p.Acquire(context.Background())
	require.NoError(t, err)
	s1 := b1.(*stubConn)
	require.NoError(t, p.Release(b1))
	require.Eventually(t, func() bool { return s1.closeCalls.Load() == 1 }, time.Second, time.Millisecond)

	b2, err := p.Acquire(context.Background())
	require.NoError(t, err)
	require.NotSame(t, b1, b2)
	require.Equal(t, int32(2), n.Load())
	require.NoError(t, p.Release(b2))
}

func TestPool_MaxIdle(t *testing.T) {
	var n atomic.Int32
	p := backend.NewPool(backend.PoolConfig{
		MaxCapacity:    4,
		MaxIdle:        1,
		AcquireTimeout: time.Second,
	}, func(context.Context) (backend.Conn, error) {
		return &stubConn{id: int(n.Add(1))}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	b1, err := p.Acquire(context.Background())
	require.NoError(t, err)
	b2, err := p.Acquire(context.Background())
	require.NoError(t, err)
	s1, s2 := b1.(*stubConn), b2.(*stubConn)

	require.NoError(t, p.Release(b1))
	require.NoError(t, p.Release(b2))
	require.False(t, s1.closed.Load(), "first idle conn is kept")
	require.True(t, s2.closed.Load(), "second idle conn exceeds MaxIdle")

	b3, err := p.Acquire(context.Background())
	require.NoError(t, err)
	require.Same(t, b1, b3)
	require.NoError(t, p.Release(b3))
}

func TestPool_Stats(t *testing.T) {
	var n atomic.Int32
	p := backend.NewPool(backend.PoolConfig{MaxCapacity: 2, AcquireTimeout: time.Second}, func(context.Context) (backend.Conn, error) {
		return &stubConn{id: int(n.Add(1))}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	require.Equal(t, backend.Stats{Max: 2}, p.Stats())
	b, err := p.Acquire(context.Background())
	require.NoError(t, err)
	require.Equal(t, backend.Stats{Active: 1, Max: 2}, p.Stats())
	require.NoError(t, p.Release(b))
	require.Equal(t, backend.Stats{Idle: 1, Max: 2}, p.Stats())
}

func TestPool_MaxLifetime(t *testing.T) {
	var n atomic.Int32
	p := backend.NewPool(backend.PoolConfig{
		MaxCapacity:    2,
		MaxLifetime:    10 * time.Millisecond,
		AcquireTimeout: time.Second,
	}, func(context.Context) (backend.Conn, error) {
		return &stubConn{id: int(n.Add(1))}, nil
	})
	t.Cleanup(func() { _ = p.Close() })

	b1, err := p.Acquire(context.Background())
	require.NoError(t, err)
	s1 := b1.(*stubConn)
	require.NoError(t, p.Release(b1))
	time.Sleep(15 * time.Millisecond)

	b2, err := p.Acquire(context.Background())
	require.NoError(t, err)
	require.NotSame(t, b1, b2)
	require.True(t, s1.closed.Load())
	require.Equal(t, int32(2), n.Load())
	require.NoError(t, p.Release(b2))
}
