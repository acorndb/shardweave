package backend

import (
	"context"
	"net"
	"testing"

	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/stretchr/testify/require"
)

func framed(payload []byte) []byte {
	buf := make([]byte, 4+len(payload))
	copy(buf[4:], payload)
	return buf
}

func writePayloadTo(t *testing.T, pc *packet.Conn, payload []byte) {
	t.Helper()
	require.NoError(t, pc.WritePacket(framed(payload)))
}

// TestRawStream_DrainedAfterOKResponse covers the reusable path: a complete
// OK response is fully consumed, so the connection may go back to the pool.
func TestRawStream_DrainedAfterOKResponse(t *testing.T) {
	backendRaw, clientRaw := net.Pipe()
	frontRaw, frontPeerRaw := net.Pipe()
	defer backendRaw.Close()
	defer clientRaw.Close()
	defer frontRaw.Close()
	defer frontPeerRaw.Close()

	srv := packet.NewConn(backendRaw)
	front := packet.NewConn(frontRaw)
	frontPeer := packet.NewConn(frontPeerRaw)

	s := &rawStream{pc: packet.NewConn(clientRaw)}
	go func() { writePayloadTo(t, srv, mysql.EncodeOK(0, 0, 0, 0)) }()

	done := make(chan error, 1)
	go func() {
		_, err := s.ForwardTo(context.Background(), front)
		done <- err
	}()

	_, err := frontPeer.ReadPacket()
	require.NoError(t, err)
	require.NoError(t, <-done)
	require.True(t, s.Drained(), "a complete response must be reusable")
}

// TestRawStream_NotDrainedOnIOError covers the discard path: a truncated
// response leaves the protocol stream in an unknown state.
func TestRawStream_NotDrainedOnIOError(t *testing.T) {
	backendRaw, clientRaw := net.Pipe()
	frontRaw, frontPeerRaw := net.Pipe()
	defer frontRaw.Close()
	defer frontPeerRaw.Close()

	_ = backendRaw.Close() // server disappears before sending anything

	s := &rawStream{pc: packet.NewConn(clientRaw)}
	_, err := s.ForwardTo(context.Background(), packet.NewConn(frontRaw))
	require.Error(t, err)
	require.False(t, s.Drained(), "an interrupted response must not be reused")
}

// TestDecodedStream_DrainedOnlyAfterEOF covers early Close: stopping before
// the row terminator leaves unread packets on the wire.
func TestDecodedStream_DrainedOnlyAfterEOF(t *testing.T) {
	t.Run("read_to_eof", func(t *testing.T) {
		backendRaw, clientRaw := net.Pipe()
		defer backendRaw.Close()
		defer clientRaw.Close()

		srv := packet.NewConn(backendRaw)
		col := mysql.Column{Name: "a", Type: 0xfd, Charset: 45}
		go func() {
			writePayloadTo(t, srv, mysql.EncodeColumnCount(1))
			writePayloadTo(t, srv, mysql.EncodeColumnDefinition(col))
			writePayloadTo(t, srv, mysql.EncodeTextRow([][]byte{[]byte("x")}))
			writePayloadTo(t, srv, mysql.EncodeEOF(true, 0, 0))
		}()

		s, err := newDecodedStream(packet.NewConn(clientRaw), true)
		require.NoError(t, err)
		require.False(t, s.Drained(), "columns alone are not a complete result")
		_, err = s.Next()
		require.NoError(t, err)
		_, err = s.Next()
		require.Error(t, err) // io.EOF
		require.True(t, s.Drained())
	})

	t.Run("early_close", func(t *testing.T) {
		backendRaw, clientRaw := net.Pipe()
		defer backendRaw.Close()
		defer clientRaw.Close()

		srv := packet.NewConn(backendRaw)
		col := mysql.Column{Name: "a", Type: 0xfd, Charset: 45}
		go func() {
			writePayloadTo(t, srv, mysql.EncodeColumnCount(1))
			writePayloadTo(t, srv, mysql.EncodeColumnDefinition(col))
			// Rows are never consumed by the test.
		}()

		s, err := newDecodedStream(packet.NewConn(clientRaw), true)
		require.NoError(t, err)
		require.NoError(t, s.Close())
		require.False(t, s.Drained(), "closing early must not mark the stream reusable")
	})
}
