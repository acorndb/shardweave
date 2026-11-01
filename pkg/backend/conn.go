package backend

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/client"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/acorndb/shardweave/pkg/result"
)

// AlignSettings is the backend session state to align before Exec.
type AlignSettings struct {
	Database string
	Charset  string
	// Vars are the replayable session variables the frontend session wants.
	// Variables present on the connection but absent here are reset to DEFAULT.
	Vars map[string]string
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
	vars     map[string]string // session variables set through alignVars
}

var _ Conn = (*MySQLConn)(nil)

// DialOptions controls backend connection timeouts and packet limits.
type DialOptions struct {
	DialTimeout      time.Duration
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	MaxAllowedPacket int
}

// DialMySQL dials addr and completes a native-password handshake.
func DialMySQL(ctx context.Context, addr, user, password, database string) (*MySQLConn, error) {
	return DialMySQLWithOptions(ctx, addr, user, password, database, DialOptions{})
}

// DialMySQLWithOptions is DialMySQL with explicit timeouts and packet limit.
func DialMySQLWithOptions(
	ctx context.Context, addr, user, password, database string, opts DialOptions,
) (*MySQLConn, error) {
	dialTimeout := 5 * time.Second
	if opts.DialTimeout > 0 {
		dialTimeout = opts.DialTimeout
	}
	d := net.Dialer{Timeout: dialTimeout}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	_ = raw.SetDeadline(time.Now().Add(dialTimeout))
	c, err := client.Dial(raw, user, password, database)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	return finishDial(raw, c, database, opts)
}

// finishDial applies the post-handshake policy and wraps the connection.
func finishDial(raw net.Conn, c *client.Conn, database string, opts DialOptions) (*MySQLConn, error) {
	// Collation-source policy (merge-engine-design §13.8 option C): stop
	// transcoding results so ColumnDefinition41 reports each column's real
	// collation instead of the connection collation. utf8mb4 data is returned
	// as-is, so the bytes clients see do not change.
	if err := setResultsNull(c); err != nil {
		_ = raw.Close()
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{})
	pc := c.PacketConn()
	pc.SetReadTimeout(opts.ReadTimeout)
	pc.SetWriteTimeout(opts.WriteTimeout)
	pc.SetMaxAllowedPacket(opts.MaxAllowedPacket)
	return &MySQLConn{pc: pc, database: database}, nil
}

// setResultsNull disables result transcoding on a freshly dialed connection.
func setResultsNull(c *client.Conn) error {
	pc := c.PacketConn()
	pc.ResetSequence()
	q := append([]byte{mysql.ComQuery}, []byte("SET character_set_results = NULL")...)
	buf := make([]byte, 4+len(q))
	copy(buf[4:], q)
	if err := pc.WritePacket(buf); err != nil {
		return err
	}
	data, err := pc.ReadPacket()
	if err != nil {
		return err
	}
	if len(data) == 0 || data[0] != mysql.OKHeader {
		return fmt.Errorf("backend: set character_set_results=NULL failed: %v", data)
	}
	return nil
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
		// SET NAMES would also reset character_set_results and undo the
		// results=NULL policy set at dial time; align the other two legs only.
		q := "SET character_set_client=" + s.Charset + ", character_set_connection=" + s.Charset
		cmd := append([]byte{mysql.ComQuery}, q...)
		if err := b.execCommand(cmd); err != nil {
			return fmt.Errorf("pool: align charset %q: %w", s.Charset, err)
		}
		b.charset = s.Charset
	}
	return b.alignVars(s.Vars)
}

// alignVars replays replayable session variables in one SET statement.
// Variables set on the connection but absent from want are reset to DEFAULT,
// so state never leaks from one frontend session to the next (Gaea-style
// unused reset). No-op when the connection already matches.
func (b *MySQLConn) alignVars(want map[string]string) error {
	if len(want) == 0 && len(b.vars) == 0 {
		return nil
	}
	var parts []string
	for name, value := range want {
		if cur, ok := b.vars[name]; !ok || cur != value {
			parts = append(parts, name+"='"+strings.ReplaceAll(value, "'", "''")+"'")
		}
	}
	for name := range b.vars {
		if _, ok := want[name]; !ok {
			parts = append(parts, name+"=DEFAULT")
		}
	}
	if len(parts) == 0 {
		return nil
	}
	sort.Strings(parts)
	cmd := append([]byte{mysql.ComQuery}, "SET "+strings.Join(parts, ", ")...)
	if err := b.execCommand(cmd); err != nil {
		return fmt.Errorf("pool: align session vars: %w", err)
	}
	b.vars = make(map[string]string, len(want))
	for name, value := range want {
		b.vars[name] = value
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
	b.vars = nil
	return nil
}

// Close closes the underlying connection.
func (b *MySQLConn) Close() error {
	if b.pc == nil {
		return nil
	}
	return b.pc.Close()
}
