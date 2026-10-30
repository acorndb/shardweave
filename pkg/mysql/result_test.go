package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEncodeOK_Minimal(t *testing.T) {
	p := EncodeOK(0, 0, 2, 0)
	require.Equal(t, byte(OKHeader), p[0])
	require.GreaterOrEqual(t, len(p), 7)
}

func TestEncodeERR(t *testing.T) {
	p := EncodeERR(1045, "28000", "Access denied")
	require.Equal(t, byte(ErrHeader), p[0])
	require.Contains(t, string(p), "Access denied")
	require.Contains(t, string(p), "#28000")
}
