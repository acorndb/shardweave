package pool

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/acorndb/prism/mysql"
	"github.com/acorndb/prism/mysql/client"
	"github.com/acorndb/prism/mysql/packet"
)

// MySQLBackend is a live backend connection after client handshake.
type MySQLBackend struct {
	conn *client.Conn
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
	return &MySQLBackend{conn: c}, nil
}

// Packet returns the framed packet connection.
func (b *MySQLBackend) Packet() *packet.Conn { return b.conn.PacketConn() }

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
	return nil
}

// Close closes the underlying connection.
func (b *MySQLBackend) Close() error {
	if b.conn == nil || b.conn.PacketConn() == nil {
		return nil
	}
	return b.conn.PacketConn().Close()
}
