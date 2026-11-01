package zerocopy_test

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/util/zerocopy"
	"github.com/stretchr/testify/require"
)

func TestHackString(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{name: "text", in: []byte("SELECT 1"), want: "SELECT 1"},
		{name: "empty", in: []byte{}, want: ""},
		{name: "nil", in: nil, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, zerocopy.HackString(tc.in))
		})
	}
}

func TestHackStringNoCopy(t *testing.T) {
	buf := []byte("SELECT * FROM t WHERE id = 1")
	var sink string
	allocs := testing.AllocsPerRun(100, func() { sink = zerocopy.HackString(buf) })
	require.Zero(t, allocs, "HackString must not copy the source")
	require.Equal(t, string(buf), sink)
}

// TestHackStringAliasesSource pins the contract: the result is a view
// rather than a copy, so callers must not mutate the source while the string
// is alive.
func TestHackStringAliasesSource(t *testing.T) {
	buf := []byte("SELECT 1")
	s := zerocopy.HackString(buf)
	buf[0] = 'X'
	require.Equal(t, "XELECT 1", s)
}
