package server_test

import (
	"net"
	"testing"

	"github.com/acorndb/shardweave/pkg/mysql/client"
	"github.com/acorndb/shardweave/pkg/mysql/server"
	"github.com/stretchr/testify/require"
)

type mapUsers map[string]string

func (m mapUsers) Password(user string) (string, bool) {
	p, ok := m[user]
	return p, ok
}

func TestHandshake_NativeRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	users := mapUsers{"root": "secret"}
	errCh := make(chan error, 1)
	go func() {
		_, err := server.NewConn(c1, users)
		errCh <- err
	}()

	cc, err := client.Dial(c2, "root", "secret", "")
	require.NoError(t, err)
	require.NotNil(t, cc)
	require.NoError(t, <-errCh)
}

func TestHandshake_WithDatabase(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	users := mapUsers{"app": "pass"}
	errCh := make(chan error, 1)
	go func() {
		sc, err := server.NewConn(c1, users)
		if err == nil {
			require.Equal(t, "testdb", sc.Database())
		}
		errCh <- err
	}()

	_, err := client.Dial(c2, "app", "pass", "testdb")
	require.NoError(t, err)
	require.NoError(t, <-errCh)
}

func TestHandshake_BadPassword(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	users := mapUsers{"root": "secret"}
	errCh := make(chan error, 1)
	go func() {
		_, err := server.NewConn(c1, users)
		errCh <- err
	}()

	_, err := client.Dial(c2, "root", "wrong", "")
	require.Error(t, err)
	require.Error(t, <-errCh)
}
