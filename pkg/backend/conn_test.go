package backend

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

func TestMySQLConn_Align_USEAndSetNames(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	srv := packet.NewConn(server)

	b := &MySQLConn{pc: packet.NewConn(client)}
	errCh := make(chan error, 1)
	go func() {
		errCh <- b.Align(context.Background(), AlignSettings{Database: "app", Charset: "utf8mb4"})
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
	go func() { errCh <- b.Align(context.Background(), AlignSettings{Database: "app", Charset: "utf8mb4"}) }()
	require.NoError(t, <-errCh)
}

func TestMySQLConn_Align_SkipsEmpty(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })

	b := &MySQLConn{pc: packet.NewConn(client)}
	require.NoError(t, b.Align(context.Background(), AlignSettings{}))
}

func TestMySQLConn_Reset_ClearsKnownSettings(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	srv := packet.NewConn(server)

	b := &MySQLConn{pc: packet.NewConn(client), database: "app", charset: "utf8mb4"}
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

func TestMySQLConn_Exec_ForwardsResponse(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	srv := packet.NewConn(server)

	b := &MySQLConn{pc: packet.NewConn(client)}
	payload := append([]byte{mysql.ComQuery}, []byte("SELECT 1")...)
	readCh := make(chan []byte, 1)
	go func() {
		srv.ResetSequence()
		data, err := srv.ReadPacket()
		if err != nil {
			readCh <- nil
			return
		}
		readCh <- data
	}()
	stream, err := b.Exec(context.Background(), ExecRequest{Payload: payload})
	require.NoError(t, err)
	require.Equal(t, payload, <-readCh)

	// Backend reply is written concurrently: ForwardTo is the reader.
	okPkt := mysql.EncodeOK(0, 0, mysql.ServerStatusAutocommit, 0)
	okBuf := make([]byte, 4+len(okPkt))
	copy(okBuf[4:], okPkt)
	go func() { _ = srv.WritePacket(okBuf) }()

	frontServer, frontClient := net.Pipe()
	t.Cleanup(func() { _ = frontServer.Close(); _ = frontClient.Close() })
	front := packet.NewConn(frontClient)
	frontRead := make(chan []byte, 1)
	go func() {
		frontSrv := packet.NewConn(frontServer)
		got, err := frontSrv.ReadPacket()
		if err != nil {
			frontRead <- nil
			return
		}
		frontRead <- got
	}()

	raw, ok := stream.(interface {
		ForwardTo(context.Context, *packet.Conn) (uint16, bool, error)
	})
	require.True(t, ok)
	status, serverErr, err := raw.ForwardTo(context.Background(), front)
	require.NoError(t, err)
	require.False(t, serverErr)
	require.Equal(t, mysql.ServerStatusAutocommit, status)
	require.NotNil(t, <-frontRead)
}

func TestMySQLConn_Exec_NoReply(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	srv := packet.NewConn(server)

	b := &MySQLConn{pc: packet.NewConn(client)}
	payload := []byte{mysql.ComStmtClose, 0x01, 0x00, 0x00, 0x00}
	readCh := make(chan []byte, 1)
	go func() {
		srv.ResetSequence()
		data, err := srv.ReadPacket()
		if err != nil {
			readCh <- nil
			return
		}
		readCh <- data
	}()
	stream, err := b.Exec(context.Background(), ExecRequest{Payload: payload, NoReply: true})
	require.NoError(t, err)
	require.Equal(t, payload, <-readCh)

	raw, ok := stream.(interface {
		ForwardTo(context.Context, *packet.Conn) (uint16, bool, error)
	})
	require.True(t, ok)
	status, serverErr, err := raw.ForwardTo(context.Background(), nil)
	require.NoError(t, err)
	require.False(t, serverErr)
	require.Equal(t, uint16(0), status)
}

func TestMySQLConn_Exec_EmptyPayload(t *testing.T) {
	b := &MySQLConn{}
	_, err := b.Exec(context.Background(), ExecRequest{})
	require.Error(t, err)
}
