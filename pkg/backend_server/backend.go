package backend_server

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/mysql/client"
	"github.com/acorndb/prism/pkg/mysql/packet"
)

// MySQLBackend is a live backend connection after client handshake.
type MySQLBackend struct {
	pc       *packet.Conn
	database string
	charset  string
}

// DialMySQL dials addr and completes a native-password handshake.
func DialMySQL(ctx context.Context, addr, user, password, database string) (*MySQLBackend, error) {
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
	return &MySQLBackend{pc: c.PacketConn(), database: database}, nil
}

// Packet returns the framed packet connection.
func (b *MySQLBackend) Packet() *packet.Conn { return b.pc }

// Database returns the last known current schema on this backend.
func (b *MySQLBackend) Database() string { return b.database }

// Charset returns the last known charset (SET NAMES) on this backend.
func (b *MySQLBackend) Charset() string { return b.charset }

// Align brings the backend session to want database/charset (Gaea-style).
// Empty want values are skipped. No-ops when already matched.
func (b *MySQLBackend) Align(ctx context.Context, database, charset string) error {
	_ = ctx
	if database != "" && database != b.database {
		cmd := make([]byte, 1+len(database))
		cmd[0] = mysql.ComInitDB
		copy(cmd[1:], database)
		if err := b.execCommand(cmd); err != nil {
			return fmt.Errorf("pool: align USE %q: %w", database, err)
		}
		b.database = database
	}
	if charset != "" && charset != b.charset {
		q := "SET NAMES " + charset
		cmd := append([]byte{mysql.ComQuery}, q...)
		if err := b.execCommand(cmd); err != nil {
			return fmt.Errorf("pool: align SET NAMES %q: %w", charset, err)
		}
		b.charset = charset
	}
	return nil
}

func (b *MySQLBackend) execCommand(cmd []byte) error {
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
func (b *MySQLBackend) Reset(ctx context.Context) error {
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
func (b *MySQLBackend) Close() error {
	if b.pc == nil {
		return nil
	}
	return b.pc.Close()
}
