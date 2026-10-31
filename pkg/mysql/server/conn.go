package server

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"net"
	"time"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/auth"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
)

// UserProvider looks up plaintext passwords for handshake (M1 local user table).
type UserProvider interface {
	Password(user string) (password string, ok bool)
}

// Options controls frontend I/O deadlines and the logical packet limit.
type Options struct {
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	MaxAllowedPacket int
}

// Conn is a server-side MySQL connection after a successful handshake.
type Conn struct {
	pc       *packet.Conn
	user     string
	database string
	scramble []byte
}

// PacketConn returns the framed connection for subsequent commands.
func (c *Conn) PacketConn() *packet.Conn { return c.pc }

// User returns the authenticated username.
func (c *Conn) User() string { return c.user }

// Database returns the schema requested in HandshakeResponse (may be empty).
func (c *Conn) Database() string { return c.database }

// NewConn performs the server-side native-password handshake on raw.
func NewConn(raw net.Conn, users UserProvider) (*Conn, error) {
	return NewConnWithOptions(raw, users, Options{})
}

// NewConnWithOptions is NewConn with frontend I/O deadlines and packet limit.
func NewConnWithOptions(raw net.Conn, users UserProvider, opts Options) (*Conn, error) {
	pc := packet.NewConn(raw)
	pc.SetReadTimeout(opts.ReadTimeout)
	pc.SetWriteTimeout(opts.WriteTimeout)
	pc.SetMaxAllowedPacket(opts.MaxAllowedPacket)
	scramble := make([]byte, 20)
	if _, err := rand.Read(scramble); err != nil {
		_ = pc.Close()
		return nil, err
	}
	// MySQL scramble bytes must be non-zero in practice; avoid 0 for simplicity.
	for i, b := range scramble {
		if b == 0 {
			scramble[i] = 1
		}
	}

	hs := &auth.HandshakeV10{
		ProtocolVersion: 10,
		ServerVersion:   "8.0.36-shardweave",
		ConnectionID:    1,
		AuthPluginData:  scramble,
		Capability: mysql.ClientProtocol41 | mysql.ClientSecureConnection |
			mysql.ClientPluginAuth | mysql.ClientTransactions |
			mysql.ClientMultiStatements | mysql.ClientMultiResults |
			mysql.ClientPSMultiResults | mysql.ClientDeprecateEOF |
			mysql.ClientSessionTrack | mysql.ClientConnectWithDB |
			mysql.ClientPluginAuthLenEncClientData,
		Collation:      255,
		Status:         0x0002, // SERVER_STATUS_AUTOCOMMIT
		AuthPluginName: auth.NativePasswordPlugin,
	}
	payload, err := hs.Encode()
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

	respData, err := pc.ReadPacket()
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	resp, err := auth.ParseHandshakeResponse41(respData, hs.Capability)
	if err != nil {
		_ = mysql.WriteERR(pc, errs.BadHandshake(err))
		_ = pc.Close()
		return nil, err
	}

	pass, ok := users.Password(resp.Username)
	if !ok {
		_ = mysql.WriteERR(pc, errs.AccessDenied(resp.Username))
		_ = pc.Close()
		return nil, fmt.Errorf("access denied for user %q", resp.Username)
	}
	expect := auth.NativePassword(pass, scramble)
	if subtle.ConstantTimeCompare(expect, resp.AuthResponse) != 1 {
		_ = mysql.WriteERR(pc, errs.AccessDenied(resp.Username))
		_ = pc.Close()
		return nil, fmt.Errorf("access denied for user %q", resp.Username)
	}

	okPkt := mysql.EncodeOK(0, 0, 0x0002, 0)
	obuf := make([]byte, 4+len(okPkt))
	copy(obuf[4:], okPkt)
	if err := pc.WritePacket(obuf); err != nil {
		_ = pc.Close()
		return nil, err
	}

	return &Conn{pc: pc, user: resp.Username, database: resp.Database, scramble: scramble}, nil
}
