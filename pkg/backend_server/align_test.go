package backend_server

import (
	"context"
	"net"
	"testing"

	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/mysql/packet"
	"github.com/stretchr/testify/require"
)

func writeOK(t *testing.T, w *packet.Conn) {
	t.Helper()
	ok := mysql.EncodeOK(0, 0, mysql.ServerStatusAutocommit, 0)
	buf := make([]byte, 4+len(ok))
	copy(buf[4:], ok)
	require.NoError(t, w.WritePacket(buf))
}

func TestMySQLBackend_Align_USEAndSetNames(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	srv := packet.NewConn(server)

	b := &MySQLBackend{pc: packet.NewConn(client)}
	errCh := make(chan error, 1)
	go func() {
		errCh <- b.Align(context.Background(), "app", "utf8mb4")
	}()

	srv.ResetSequence()
	data, err := srv.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, append([]byte{mysql.ComInitDB}, []byte("app")...), data)
	writeOK(t, srv)

	srv.ResetSequence()
	data, err = srv.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, append([]byte{mysql.ComQuery}, []byte("SET NAMES utf8mb4")...), data)
	writeOK(t, srv)

	require.NoError(t, <-errCh)
	require.Equal(t, "app", b.Database())
	require.Equal(t, "utf8mb4", b.Charset())

	// Same settings → no commands.
	go func() { errCh <- b.Align(context.Background(), "app", "utf8mb4") }()
	require.NoError(t, <-errCh)
}

func TestMySQLBackend_Align_SkipsEmpty(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })

	b := &MySQLBackend{pc: packet.NewConn(client)}
	require.NoError(t, b.Align(context.Background(), "", ""))
}

func TestMySQLBackend_Reset_ClearsKnownSettings(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	srv := packet.NewConn(server)

	b := &MySQLBackend{pc: packet.NewConn(client), database: "app", charset: "utf8mb4"}
	errCh := make(chan error, 1)
	go func() {
		errCh <- b.Reset(context.Background())
	}()

	srv.ResetSequence()
	data, err := srv.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, []byte{mysql.ComResetConnection}, data)
	writeOK(t, srv)

	require.NoError(t, <-errCh)
	require.Equal(t, "", b.Database())
	require.Equal(t, "", b.Charset())
}
