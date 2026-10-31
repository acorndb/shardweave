package packet_test

import (
	"strconv"
	"testing"

	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/stretchr/testify/require"
)

func TestLenEncInt_RoundTrip(t *testing.T) {
	cases := []uint64{0, 250, 251, 252, 0xffff, 0xffffff, 0x1000000, ^uint64(0)}
	for _, n := range cases {
		t.Run(strconv.FormatUint(n, 10), func(t *testing.T) {
			buf := packet.AppendLenEncInt(nil, n)
			got, adv, isNull, err := packet.ReadLenEncInt(buf)
			require.NoError(t, err)
			require.False(t, isNull)
			require.Equal(t, n, got)
			require.Equal(t, len(buf), adv)
		})
	}
}

func TestLenEncInt_Null(t *testing.T) {
	buf := []byte{0xfb}
	_, adv, isNull, err := packet.ReadLenEncInt(buf)
	require.NoError(t, err)
	require.True(t, isNull)
	require.Equal(t, 1, adv)
}

func TestLenEncString_RoundTrip(t *testing.T) {
	buf := packet.AppendLenEncString(nil, "hello")
	s, adv, err := packet.ReadLenEncString(buf)
	require.NoError(t, err)
	require.Equal(t, "hello", s)
	require.Equal(t, len(buf), adv)
}

func TestLenEncInt_Truncated(t *testing.T) {
	_, _, _, err := packet.ReadLenEncInt(nil)
	require.Error(t, err)
	_, _, _, err = packet.ReadLenEncInt([]byte{0xfc, 0x01})
	require.Error(t, err)
}
