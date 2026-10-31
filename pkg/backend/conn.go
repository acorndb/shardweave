package backend

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/mysql/client"
	"github.com/acorndb/prism/pkg/mysql/packet"
	"github.com/acorndb/prism/pkg/result"
)

// AlignSettings is the backend session state to align before Exec.
type AlignSettings struct {
	Database string
	Charset  string
}

// ExecRequest is one full command packet plus its expected response shape.
type ExecRequest struct {
	Payload []byte // full command packet, including the command byte
	Prepare bool   // COM_STMT_PREPARE response
	NoReply bool   // COM_STMT_CLOSE and other no-response commands
}

// Conn is a pooled MySQL backend connection.
type Conn interface {
	Ping(ctx context.Context) error
	Exec(ctx context.Context, req ExecRequest) (result.Stream, error)
	Align(ctx context.Context, s AlignSettings) error
	Reset(ctx context.Context) error
	Close() error
}

// MySQLConn is a live backend connection after client handshake.
type MySQLConn struct {
	pc       *packet.Conn
	database string
	charset  string
}

var _ Conn = (*MySQLConn)(nil)

// DialMySQL dials addr and completes a native-password handshake.
func DialMySQL(ctx context.Context, addr, user, password, database string) (*MySQLConn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c, err := client.Dial(raw, user, password, database)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	return &MySQLConn{pc: c.PacketConn(), database: database}, nil
}

// Packet returns the framed packet connection.
func (b *MySQLConn) Packet() *packet.Conn { return b.pc }

// Database returns the last known current schema on this backend.
func (b *MySQLConn) Database() string { return b.database }

// Charset returns the last known charset (SET NAMES) on this backend.
func (b *MySQLConn) Charset() string { return b.charset }

// Ping sends COM_PING and expects OK.
func (b *MySQLConn) Ping(ctx context.Context) error {
	_ = ctx
	return b.execCommand([]byte{mysql.ComPing})
}

// Exec sends one command packet and returns its response stream.
func (b *MySQLConn) Exec(ctx context.Context, req ExecRequest) (result.Stream, error) {
	_ = ctx
	if len(req.Payload) == 0 {
		return nil, fmt.Errorf("backend: empty command payload")
	}
	pc := b.Packet()
	pc.ResetSequence()
	buf := make([]byte, 4+len(req.Payload))
	copy(buf[4:], req.Payload)
	if err := pc.WritePacket(buf); err != nil {
		return nil, err
	}
	if req.NoReply {
		return noReplyStream{}, nil
	}
	return &rawStream{pc: pc, prepare: req.Prepare}, nil
}

// Align brings the backend session to want database/charset (Gaea-style).
// Empty want values are skipped. No-ops when already matched.
func (b *MySQLConn) Align(ctx context.Context, s AlignSettings) error {
	_ = ctx
	if s.Database != "" && s.Database != b.database {
		cmd := make([]byte, 1+len(s.Database))
		cmd[0] = mysql.ComInitDB
		copy(cmd[1:], s.Database)
		if err := b.execCommand(cmd); err != nil {
			return fmt.Errorf("pool: align USE %q: %w", s.Database, err)
		}
		b.database = s.Database
	}
	if s.Charset != "" && s.Charset != b.charset {
		q := "SET NAMES " + s.Charset
		cmd := append([]byte{mysql.ComQuery}, q...)
		if err := b.execCommand(cmd); err != nil {
			return fmt.Errorf("pool: align SET NAMES %q: %w", s.Charset, err)
		}
		b.charset = s.Charset
	}
	return nil
}

func (b *MySQLConn) execCommand(cmd []byte) error {
	pc := b.Packet()
	pc.ResetSequence()
	buf := make([]byte, 4+len(cmd))
	copy(buf[4:], cmd)
	if err := pc.WritePacket(buf); err != nil {
		return err
	}
	data, err := pc.ReadPacket()
	if err != nil {
		return err
	}
	if len(data) == 0 || data[0] != mysql.OKHeader {
		return fmt.Errorf("pool: expected OK, got %v", data)
	}
	return nil
}

// Reset sends COM_RESET_CONNECTION and expects OK (MySQL 5.7+).
func (b *MySQLConn) Reset(ctx context.Context) error {
	_ = ctx
	pc := b.Packet()
	pc.ResetSequence()
	buf := []byte{0, 0, 0, 0, mysql.ComResetConnection}
	if err := pc.WritePacket(buf); err != nil {
		return err
	}
	data, err := pc.ReadPacket()
	if err != nil {
		return err
	}
	if len(data) == 0 || data[0] != mysql.OKHeader {
		return fmt.Errorf("pool: reset expected OK, got %v", data)
	}
	b.database = ""
	b.charset = ""
	return nil
}

// Close closes the underlying connection.
func (b *MySQLConn) Close() error {
	if b.pc == nil {
		return nil
	}
	return b.pc.Close()
}
