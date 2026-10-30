package proxy

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/acorndb/prism/pkg/backend_server"
	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/mysql/packet"
	"github.com/acorndb/prism/pkg/mysql/server"
	"github.com/acorndb/prism/pkg/router"
	"github.com/acorndb/prism/pkg/session"
)

// SessionConfig wires frontend auth and backend topology for passthrough.
type SessionConfig struct {
	Users server.UserProvider
	Group *router.Group
}

// Serve runs one frontend connection: handshake then command loop.
func Serve(ctx context.Context, raw net.Conn, cfg SessionConfig) error {
	if cfg.Group == nil {
		return fmt.Errorf("proxy: nil backend group")
	}
	sc, err := server.NewConn(raw, cfg.Users)
	if err != nil {
		return err
	}
	front := sc.PacketConn()
	st := session.NewState()
	if db := sc.Database(); db != "" {
		st.SetDatabase(db)
	}

	var sticky backend_server.Backend
	var stickyPool *backend_server.Pool
	defer func() {
		if sticky != nil {
			_ = cfg.Group.Discard(stickyPool, sticky)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		front.ResetSequence()
		data, err := front.ReadPacket()
		if err != nil {
			return err
		}
		if len(data) == 0 {
			continue
		}

		cmd := data[0]
		sql := ""
		if cmd == mysql.ComQuery && len(data) > 1 {
			sql = string(data[1:])
		}

		switch cmd {
		case mysql.ComQuit:
			return nil
		case mysql.ComPing:
			if err := writeOK(front); err != nil {
				return err
			}
		case mysql.ComInitDB:
			db := string(data[1:])
			st.SetDatabase(db)
			if err := withBackend(ctx, cfg.Group, st, cmd, sql, &sticky, &stickyPool, func(b backend_server.Backend) error {
				return execCommand(front, b, st, data, false)
			}); err != nil {
				return err
			}
		case mysql.ComQuery:
			if session.DetectReservedSQL(sql) {
				st.MarkReserved()
			}
			if db, ok := parseUse(sql); ok {
				st.SetDatabase(db)
			}
			if cs, ok := parseSetNames(sql); ok {
				st.SetCharset(cs)
			}
			if v, ok := parseSetAutocommit(sql); ok {
				st.SetAutocommit(v)
			}
			if err := withBackend(ctx, cfg.Group, st, cmd, sql, &sticky, &stickyPool, func(b backend_server.Backend) error {
				return execCommand(front, b, st, data, false)
			}); err != nil {
				return err
			}
		case mysql.ComStmtPrepare:
			st.MarkReserved()
			if err := withBackend(ctx, cfg.Group, st, cmd, sql, &sticky, &stickyPool, func(b backend_server.Backend) error {
				return execCommand(front, b, st, data, true)
			}); err != nil {
				return err
			}
		case mysql.ComStmtExecute, mysql.ComStmtReset:
			if err := withBackend(ctx, cfg.Group, st, cmd, sql, &sticky, &stickyPool, func(b backend_server.Backend) error {
				return execCommand(front, b, st, data, false)
			}); err != nil {
				return err
			}
		case mysql.ComStmtClose:
			if err := withBackend(ctx, cfg.Group, st, cmd, sql, &sticky, &stickyPool, func(b backend_server.Backend) error {
				return writeBackend(b, data)
			}); err != nil {
				return err
			}
		case mysql.ComResetConnection:
			if err := handleComResetConnection(ctx, cfg.Group, st, &sticky, &stickyPool); err != nil {
				return err
			}
			if err := writeOK(front); err != nil {
				return err
			}
		default:
			_ = writeERR(front, 1047, "08S01", fmt.Sprintf("command %d not supported yet", data[0]))
		}
	}
}

func parseUse(sql string) (string, bool) {
	s := strings.TrimSpace(sql)
	if len(s) < 4 {
		return "", false
	}
	if !strings.EqualFold(s[:3], "USE") {
		return "", false
	}
	rest := strings.TrimSpace(s[3:])
	rest = strings.TrimSuffix(rest, ";")
	rest = strings.TrimSpace(rest)
	quoteChars := string([]byte{'`', '"', '\''})
	rest = strings.Trim(rest, quoteChars)
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false
	}
	return rest, true
}

func parseSetNames(sql string) (string, bool) {
	s := strings.TrimSpace(sql)
	s = strings.TrimSuffix(s, ";")
	s = strings.TrimSpace(s)
	upper := strings.ToUpper(s)
	const prefix = "SET NAMES"
	if !strings.HasPrefix(upper, prefix) {
		return "", false
	}
	rest := strings.TrimSpace(s[len(prefix):])
	if rest == "" {
		return "", false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", false
	}
	cs := strings.Trim(fields[0], "`\"'")
	if cs == "" {
		return "", false
	}
	return cs, true
}

// parseSetAutocommit recognizes SET autocommit = 0|1|ON|OFF (design §5.9 / kingshard).
func parseSetAutocommit(sql string) (bool, bool) {
	s := strings.TrimSpace(sql)
	s = strings.TrimSuffix(s, ";")
	s = strings.TrimSpace(s)
	upper := strings.ToUpper(s)
	if !strings.HasPrefix(upper, "SET") {
		return false, false
	}
	rest := strings.TrimSpace(s[3:])
	// strip optional SESSION.
	if len(rest) >= 7 && strings.EqualFold(rest[:7], "SESSION") {
		rest = strings.TrimSpace(rest[7:])
	}
	eq := strings.IndexByte(rest, '=')
	if eq < 0 {
		return false, false
	}
	name := strings.TrimSpace(rest[:eq])
	name = strings.TrimPrefix(strings.ToLower(name), "@@")
	name = strings.TrimPrefix(name, "session.")
	if !strings.EqualFold(name, "autocommit") {
		return false, false
	}
	val := strings.TrimSpace(rest[eq+1:])
	val = strings.Trim(val, "`\"'")
	switch strings.ToUpper(val) {
	case "1", "ON":
		return true, true
	case "0", "OFF":
		return false, true
	default:
		return false, false
	}
}

func handleComResetConnection(ctx context.Context, g *router.Group, st *session.State, sticky *backend_server.Backend, stickyPool **backend_server.Pool) error {
	_ = ctx
	if *sticky != nil {
		b := *sticky
		p := *stickyPool
		*sticky = nil
		*stickyPool = nil
		if err := g.Release(p, b); err != nil {
			st.Reset()
			return nil
		}
	}
	st.Reset()
	return nil
}

func withBackend(
	ctx context.Context,
	g *router.Group,
	st *session.State,
	cmd byte,
	sql string,
	sticky *backend_server.Backend,
	stickyPool **backend_server.Pool,
	fn func(backend_server.Backend) error,
) error {
	var b backend_server.Backend
	var p *backend_server.Pool
	var err error

	if *sticky != nil {
		b = *sticky
		p = *stickyPool
	} else {
		role := router.Pick(st, cmd, sql)
		b, p, err = g.Acquire(ctx, role)
		if err != nil {
			return err
		}
	}

	if err := alignBackend(ctx, b, st); err != nil {
		if *sticky == nil {
			_ = g.Release(p, b)
		}
		return err
	}

	ferr := fn(b)

	if st.NeedsSticky() {
		*sticky = b
		*stickyPool = p
		return ferr
	}
	*sticky = nil
	*stickyPool = nil
	_ = g.Release(p, b)
	return ferr
}

func alignBackend(ctx context.Context, b backend_server.Backend, st *session.State) error {
	a, ok := b.(interface {
		Align(context.Context, string, string) error
	})
	if !ok {
		return nil
	}
	return a.Align(ctx, st.Database(), st.Charset())
}

func writeBackend(b backend_server.Backend, cmd []byte) error {
	mb, ok := b.(*backend_server.MySQLBackend)
	if !ok {
		return fmt.Errorf("proxy: backend type %T not supported", b)
	}
	back := mb.Packet()
	back.ResetSequence()
	buf := make([]byte, 4+len(cmd))
	copy(buf[4:], cmd)
	return back.WritePacket(buf)
}

func execCommand(front *packet.Conn, b backend_server.Backend, st *session.State, cmd []byte, prepare bool) error {
	mb, ok := b.(*backend_server.MySQLBackend)
	if !ok {
		return fmt.Errorf("proxy: backend type %T not supported", b)
	}
	back := mb.Packet()
	back.ResetSequence()
	buf := make([]byte, 4+len(cmd))
	copy(buf[4:], cmd)
	if err := back.WritePacket(buf); err != nil {
		return err
	}
	const deprecateEOF = true
	if prepare {
		return mysql.ForwardPrepareResponse(front, back, deprecateEOF)
	}
	status, err := mysql.ForwardResponse(front, back, deprecateEOF)
	if err != nil {
		return err
	}
	st.SetInTx(status&mysql.ServerStatusInTrans != 0)
	return nil
}

func writeOK(front *packet.Conn) error {
	p := mysql.EncodeOK(0, 0, mysql.ServerStatusAutocommit, 0)
	buf := make([]byte, 4+len(p))
	copy(buf[4:], p)
	return front.WritePacket(buf)
}

func writeERR(front *packet.Conn, errno uint16, state, msg string) error {
	p := mysql.EncodeERR(errno, state, msg)
	buf := make([]byte, 4+len(p))
	copy(buf[4:], p)
	return front.WritePacket(buf)
}
