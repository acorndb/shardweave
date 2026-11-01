package backend

import (
	"bytes"
	"context"
	"net"
	"testing"

	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/auth"
	"github.com/acorndb/shardweave/pkg/mysql/client"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/stretchr/testify/require"
)

func writeOK(t *testing.T, w *packet.Conn) {
	t.Helper()
	ok := mysql.EncodeOK(0, 0, mysql.ServerStatusAutocommit, 0)
	buf := make([]byte, 4+len(ok))
	copy(buf[4:], ok)
	require.NoError(t, w.WritePacket(buf))
}

// TestDialMySQLWithOptions_SetsResultsNull covers the collation-source policy
// (merge-engine-design §13.8 option C): after the handshake the backend must
// stop transcoding results so ColumnDefinition41 carries each column's real
// collation instead of the connection collation.
func TestDialMySQLWithOptions_SetsResultsNull(t *testing.T) {
	server, clientEnd := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = clientEnd.Close() })
	srv := packet.NewConn(server)

	got := make(chan []byte, 1)
	go func() {
		hs := &auth.HandshakeV10{
			ProtocolVersion: 10,
			ServerVersion:   "8.0.36",
			ConnectionID:    1,
			AuthPluginData:  bytes.Repeat([]byte{1}, 20),
			Capability: mysql.ClientProtocol41 | mysql.ClientSecureConnection |
				mysql.ClientPluginAuth,
			Collation:      45,
			Status:         0x0002,
			AuthPluginName: auth.NativePasswordPlugin,
		}
		payload, err := hs.Encode()
		if err != nil {
			got <- nil
			return
		}
		framed := make([]byte, 4+len(payload))
		copy(framed[4:], payload)
		if err := srv.WritePacket(framed); err != nil {
			got <- nil
			return
		}
		if _, err := srv.ReadPacket(); err != nil { // HandshakeResponse41
			got <- nil
			return
		}
		writeOK(t, srv)
		srv.ResetSequence()
		data, err := srv.ReadPacket()
		if err != nil {
			got <- nil
			return
		}
		writeOK(t, srv)
		got <- data
	}()

	c, err := client.Dial(clientEnd, "u", "p", "")
	require.NoError(t, err)
	b, err := finishDial(clientEnd, c, "", DialOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	want := append([]byte{mysql.ComQuery}, []byte("SET character_set_results = NULL")...)
	require.Equal(t, want, <-got)
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
	// SET NAMES would reset character_set_results and undo the results=NULL
	// policy set at dial time, so charset alignment only sets the other legs.
	require.Equal(t,
		append([]byte{mysql.ComQuery}, []byte("SET character_set_client=utf8mb4, character_set_connection=utf8mb4")...),
		data)
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

func TestMySQLConn_Align_Vars(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	srv := packet.NewConn(server)

	b := &MySQLConn{pc: packet.NewConn(client)}
	errCh := make(chan error, 1)

	expectSet := func(want string) {
		t.Helper()
		srv.ResetSequence()
		data, err := srv.ReadPacket()
		require.NoError(t, err)
		require.Equal(t, append([]byte{mysql.ComQuery}, []byte(want)...), data)
		writeOK(t, srv)
		require.NoError(t, <-errCh)
	}

	// Set: empty connection + wanted var → one SET.
	go func() {
		errCh <- b.Align(context.Background(), AlignSettings{Vars: map[string]string{"time_zone": "+08:00"}})
	}()
	expectSet("SET time_zone='+08:00'")

	// Same value → no command.
	go func() {
		errCh <- b.Align(context.Background(), AlignSettings{Vars: map[string]string{"time_zone": "+08:00"}})
	}()
	require.NoError(t, <-errCh)

	// Update.
	go func() {
		errCh <- b.Align(context.Background(), AlignSettings{Vars: map[string]string{"time_zone": "+00:00"}})
	}()
	expectSet("SET time_zone='+00:00'")

	// Removed on the session → reset to DEFAULT on the connection.
	go func() {
		errCh <- b.Align(context.Background(), AlignSettings{})
	}()
	expectSet("SET time_zone=DEFAULT")
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
		ForwardTo(context.Context, *packet.Conn) (mysql.ResponseInfo, error)
	})
	require.True(t, ok)
	info, err := raw.ForwardTo(context.Background(), front)
	require.NoError(t, err)
	require.False(t, info.ServerErr)
	require.Equal(t, mysql.ServerStatusAutocommit, info.Status)
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
		ForwardTo(context.Context, *packet.Conn) (mysql.ResponseInfo, error)
	})
	require.True(t, ok)
	info, err := raw.ForwardTo(context.Background(), nil)
	require.NoError(t, err)
	require.False(t, info.ServerErr)
	require.Equal(t, uint16(0), info.Status)
}

func TestMySQLConn_Exec_EmptyPayload(t *testing.T) {
	b := &MySQLConn{}
	_, err := b.Exec(context.Background(), ExecRequest{})
	require.Error(t, err)
}

func TestMySQLConn_Ping(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	srv := packet.NewConn(server)

	b := &MySQLConn{pc: packet.NewConn(client)}
	errCh := make(chan error, 1)
	go func() {
		errCh <- b.Ping(context.Background())
	}()

	srv.ResetSequence()
	data, err := srv.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, []byte{mysql.ComPing}, data)
	writeOK(t, srv)

	require.NoError(t, <-errCh)
}
