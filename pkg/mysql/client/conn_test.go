package client

import (
	"bytes"
	"net"
	"testing"

	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/auth"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/stretchr/testify/require"
)

func writeFramed(pc *packet.Conn, payload []byte) error {
	buf := make([]byte, 4+len(payload))
	copy(buf[4:], payload)
	return pc.WritePacket(buf)
}

// TestDial_PinsUTF8mb4Collation verifies that the proxy does not echo the
// backend server's default collation into HandshakeResponse41. Result bytes
// must always arrive as utf8mb4 so merge can apply utf8mb4 collators; the
// frontend-facing policy rejects non-utf8mb4 SET NAMES (design §13.4).
func TestDial_PinsUTF8mb4Collation(t *testing.T) {
	cli, srv := net.Pipe()

	collation := make(chan uint8, 1)
	serverErr := make(chan error, 1)
	go func() {
		defer srv.Close()
		pc := packet.NewConn(srv)
		hs := &auth.HandshakeV10{
			ProtocolVersion: 10,
			ServerVersion:   "8.0.36",
			ConnectionID:    1,
			AuthPluginData:  bytes.Repeat([]byte{1}, 20),
			Capability: mysql.ClientProtocol41 | mysql.ClientSecureConnection |
				mysql.ClientPluginAuth,
			// 33 = utf8_general_ci: a non-utf8mb4 server default must not leak.
			Collation:      33,
			Status:         0x0002,
			AuthPluginName: auth.NativePasswordPlugin,
		}
		payload, err := hs.Encode()
		if err != nil {
			serverErr <- err
			return
		}
		if err := writeFramed(pc, payload); err != nil {
			serverErr <- err
			return
		}
		data, err := pc.ReadPacket()
		if err != nil {
			serverErr <- err
			return
		}
		resp, err := auth.ParseHandshakeResponse41(data, hs.Capability)
		if err != nil {
			serverErr <- err
			return
		}
		collation <- resp.Collation
		ok := []byte{mysql.OKHeader, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}
		if err := writeFramed(pc, ok); err != nil {
			serverErr <- err
		}
	}()

	c, err := Dial(cli, "user", "pass", "")
	require.NoError(t, err)
	require.NoError(t, c.PacketConn().Close())

	select {
	case got := <-collation:
		require.Equal(t, uint8(45), got, "handshake must pin utf8mb4_general_ci (45)")
	case err := <-serverErr:
		t.Fatalf("fake server: %v", err)
	}
}
