// Internal tests for unexported routing helpers; the public routing surface is
// covered by router_test.go (package shard_test).
package shard

import (
	"hash/fnv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFNVSumMatchesHashFNV(t *testing.T) {
	inputs := []string{"", "a", "orders_42", "order-2026-0001"}
	for _, s := range inputs {
		h := fnv.New64a()
		_, _ = h.Write([]byte(s))
		require.Equal(t, h.Sum64(), fnvSum(s), "string %q", s)
		require.Equal(t, h.Sum64(), fnvSum([]byte(s)), "bytes %q", s)
	}
}

func TestFNVSumStringNoAlloc(t *testing.T) {
	const key = "order-2026-0001"
	var sink uint64
	allocs := testing.AllocsPerRun(100, func() { sink = fnvSum(key) })
	require.Zero(t, allocs, "hashing a string key must not allocate")
	require.NotZero(t, sink)
}
