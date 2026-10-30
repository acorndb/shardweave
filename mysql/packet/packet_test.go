package packet_test

import (
	"io"
	"net"
	"testing"

	"github.com/acorndb/prism/mysql/packet"
	"github.com/stretchr/testify/require"
)

func TestConn_WriteReadRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	server := packet.NewConn(c1)
	client := packet.NewConn(c2)

	payload := []byte("ping-payload")
	buf := make([]byte, 4+len(payload))
	copy(buf[4:], payload)

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.WritePacket(buf)
	}()

	got, err := client.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, payload, got)
	require.NoError(t, <-errCh)
	require.Equal(t, uint8(1), client.Sequence)
	require.Equal(t, uint8(1), server.Sequence)
}

func TestConn_SequenceMismatch(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })

	reader := packet.NewConn(c1)
	errCh := make(chan error, 1)
	go func() {
		// zero-length packet, wrong sequence — no payload to drain
		_, err := c2.Write([]byte{0, 0, 0, 5})
		errCh <- err
	}()

	_, err := reader.ReadPacket()
	require.Error(t, err)
	require.Contains(t, err.Error(), "sequence")
	require.NoError(t, <-errCh)
}

func TestConn_ResetSequence(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	w := packet.NewConn(c1)
	r := packet.NewConn(c2)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.ReadPacket()
	}()
	require.NoError(t, w.WritePacket([]byte{0, 0, 0, 0, 1}))
	<-done
	require.Equal(t, uint8(1), w.Sequence)
	w.ResetSequence()
	require.Equal(t, uint8(0), w.Sequence)
}

func TestConn_EmptyPayload(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	w := packet.NewConn(c1)
	r := packet.NewConn(c2)
	errCh := make(chan error, 1)
	go func() { errCh <- w.WritePacket(make([]byte, 4)) }()
	got, err := r.ReadPacket()
	require.NoError(t, err)
	require.Empty(t, got)
	require.NoError(t, <-errCh)
}

func TestConn_ReadEOF(t *testing.T) {
	c1, c2 := net.Pipe()
	_ = c2.Close()
	r := packet.NewConn(c1)
	_, err := r.ReadPacket()
	require.ErrorIs(t, err, io.EOF)
}

func TestConn_WritePacketTooShort(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	w := packet.NewConn(c1)
	require.Error(t, w.WritePacket([]byte{1, 2, 3}))
}
