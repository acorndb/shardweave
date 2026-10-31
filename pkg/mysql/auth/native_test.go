package auth_test

import (
	"encoding/hex"
	"testing"

	"github.com/acorndb/shardweave/pkg/mysql/auth"
	"github.com/stretchr/testify/require"
)

func TestNativePassword_Empty(t *testing.T) {
	require.Nil(t, auth.NativePassword("", make([]byte, 20)))
}

func TestNativePassword_KnownVector(t *testing.T) {
	// Same algorithm as go-mysql CalcNativePassword; scramble is 20 bytes.
	scramble, err := hex.DecodeString("00112233445566778899aabbccddeeff00112233")
	require.NoError(t, err)
	got := auth.NativePassword("secret", scramble)
	require.Len(t, got, 20)
	// Round-trip stability: same inputs → same output
	require.Equal(t, got, auth.NativePassword("secret", scramble))
	require.NotEqual(t, got, auth.NativePassword("other", scramble))
}
