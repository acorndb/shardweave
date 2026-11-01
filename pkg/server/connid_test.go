package server

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConnIDAllocator_Sequential(t *testing.T) {
	var a connIDAllocator
	require.Equal(t, uint32(1), a.alloc())
	require.Equal(t, uint32(2), a.alloc())
}

func TestConnIDAllocator_FreeIsNotReusedBeforeWrap(t *testing.T) {
	var a connIDAllocator
	first := a.alloc()
	a.free(first)
	require.Equal(t, uint32(2), a.alloc(), "MySQL only reuses ids after the counter wraps")
}

func TestConnIDAllocator_Wrap(t *testing.T) {
	cases := []struct {
		name string
		next uint32
		live []uint32
		want uint32
	}{
		{name: "skips_zero", next: 0xFFFFFFFF, want: 1},
		{name: "skips_live", next: 0xFFFFFFFF, live: []uint32{1, 2}, want: 3},
		{name: "skips_zero_and_live", next: 0xFFFFFFFE, live: []uint32{0xFFFFFFFF}, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := connIDAllocator{next: tc.next}
			if len(tc.live) > 0 {
				a.live = make(map[uint32]struct{}, len(tc.live))
				for _, id := range tc.live {
					a.live[id] = struct{}{}
				}
			}
			require.Equal(t, tc.want, a.alloc())
		})
	}
}

func TestConnIDAllocator_ConcurrentUnique(t *testing.T) {
	var a connIDAllocator
	const n = 128
	ids := make(chan uint32, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- a.alloc()
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[uint32]struct{}, n)
	for id := range ids {
		require.NotZero(t, id)
		_, dup := seen[id]
		require.False(t, dup, "duplicate connection id %d", id)
		seen[id] = struct{}{}
	}
	require.Len(t, seen, n)
}
