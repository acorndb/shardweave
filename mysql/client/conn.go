package client

import (
	"fmt"
	"net"

	"github.com/acorndb/prism/mysql"
	"github.com/acorndb/prism/mysql/auth"
	"github.com/acorndb/prism/mysql/packet"
)

// Conn is a client-side MySQL connection after a successful handshake.
type Conn struct {
	pc *packet.Conn
}

// PacketConn returns the framed connection.
func (c *Conn) PacketConn() *packet.Conn { return c.pc }

// Dial performs a native-password handshake on an existing net.Conn
// (for tests use net.Pipe; production will dial TCP then Dial).
func Dial(raw net.Conn, user, password, database string) (*Conn, error) {
	pc := packet.NewConn(raw)

	data, err := pc.ReadPacket()
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	if len(data) > 0 && data[0] == mysql.ErrHeader {
		_ = pc.Close()
		return nil, fmt.Errorf("server error during handshake")
	}

	hs, err := auth.ParseHandshakeV10(data)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}

	scramble := hs.AuthPluginData
	if len(scramble) > 20 {
		scramble = scramble[:20]
	}
	authResp := auth.NativePassword(password, scramble)

	resp := &auth.HandshakeResponse41{
		Capability: hs.Capability & (mysql.ClientProtocol41 | mysql.ClientSecureConnection |
			mysql.ClientPluginAuth | mysql.ClientPluginAuthLenEncClientData |
			mysql.ClientConnectWithDB | mysql.ClientTransactions |
			mysql.ClientMultiResults | mysql.ClientDeprecateEOF),
		Collation:      hs.Collation,
		Username:       user,
		AuthResponse:   authResp,
		Database:       database,
		AuthPluginName: auth.NativePasswordPlugin,
	}
	payload, err := resp.Encode()
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	buf := make([]byte, 4+len(payload))
	copy(buf[4:], payload)
	if err := pc.WritePacket(buf); err != nil {
		_ = pc.Close()
		return nil, err
	}

	reply, err := pc.ReadPacket()
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	if len(reply) == 0 {
		_ = pc.Close()
		return nil, fmt.Errorf("empty handshake reply")
	}
	switch reply[0] {
	case mysql.OKHeader:
		return &Conn{pc: pc}, nil
	case mysql.ErrHeader:
		_ = pc.Close()
		return nil, fmt.Errorf("access denied")
	default:
		_ = pc.Close()
		return nil, fmt.Errorf("unexpected handshake reply header 0x%02x", reply[0])
	}
}
