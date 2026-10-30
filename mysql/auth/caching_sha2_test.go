package auth_test

import (
	"encoding/hex"
	"testing"

	"github.com/sharding-proxy/mysql/auth"
	"github.com/stretchr/testify/require"
)

func TestCachingSHA2Password_Empty(t *testing.T) {
	require.Nil(t, auth.CachingSHA2Password("", make([]byte, 20)))
}

func TestCachingSHA2Password_KnownVector(t *testing.T) {
	scramble, err := hex.DecodeString("00112233445566778899aabbccddeeff00112233")
	require.NoError(t, err)
	got := auth.CachingSHA2Password("secret", scramble)
	require.Len(t, got, 32)
	require.Equal(t, got, auth.CachingSHA2Password("secret", scramble))
	require.NotEqual(t, got, auth.CachingSHA2Password("other", scramble))
}
