package server_test

import (
	"net"
	"testing"

	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/auth"
	"github.com/acorndb/shardweave/pkg/mysql/client"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
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

func TestHandshake_AdvertisesConnectionID(t *testing.T) {
	cases := []struct {
		name string
		id   uint32
		want uint32
	}{
		{name: "explicit", id: 42, want: 42},
		{name: "zero_defaults_to_one", id: 0, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c1, c2 := net.Pipe()
			t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

			errCh := make(chan error, 1)
			go func() {
				_, err := server.NewConnWithOptions(c1, mapUsers{"root": "secret"}, server.Options{ConnectionID: tc.id})
				errCh <- err
			}()

			data, err := packet.NewConn(c2).ReadPacket()
			require.NoError(t, err)
			hs, err := auth.ParseHandshakeV10(data)
			require.NoError(t, err)
			require.Equal(t, tc.want, hs.ConnectionID)
			require.Equal(t, mysql.ServerStatusAutocommit, hs.Status)

			_ = c2.Close()
			require.Error(t, <-errCh, "closing before the handshake response must fail the server side")
		})
	}
}

func TestHandshake_ConnectionIDAccessor(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	connCh := make(chan *server.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		sc, err := server.NewConnWithOptions(c1, mapUsers{"app": "pass"}, server.Options{ConnectionID: 7})
		if err != nil {
			errCh <- err
			return
		}
		connCh <- sc
		errCh <- nil
	}()

	_, err := client.Dial(c2, "app", "pass", "")
	require.NoError(t, err)
	require.Equal(t, uint32(7), (<-connCh).ConnectionID())
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
