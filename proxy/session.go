package proxy

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/acorndb/prism/mysql"
	"github.com/acorndb/prism/mysql/packet"
	"github.com/acorndb/prism/mysql/server"
	"github.com/acorndb/prism/pool"
	"github.com/acorndb/prism/session"
)

// SessionConfig wires frontend auth and backend pool for passthrough.
type SessionConfig struct {
	Users server.UserProvider
	Pool  *pool.Pool
}

// Serve runs one frontend connection: handshake then command loop.
func Serve(ctx context.Context, raw net.Conn, cfg SessionConfig) error {
	sc, err := server.NewConn(raw, cfg.Users)
	if err != nil {
		return err
	}
	front := sc.PacketConn()
	st := session.NewState()
	if db := sc.Database(); db != "" {
		st.SetDatabase(db)
	}

	var sticky pool.Backend
	defer func() {
		if sticky != nil {
			_ = sticky.Close()
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

		switch data[0] {
		case mysql.ComQuit:
			return nil
		case mysql.ComPing:
			if err := writeOK(front); err != nil {
				return err
			}
		case mysql.ComInitDB:
			db := string(data[1:])
			st.SetDatabase(db)
			if err := withBackend(ctx, cfg.Pool, st, &sticky, func(b pool.Backend) error {
				return execCommand(front, b, data, false)
			}); err != nil {
				return err
			}
		case mysql.ComQuery:
			sql := string(data[1:])
			if session.DetectReservedSQL(sql) {
				st.MarkReserved()
			}
			if db, ok := parseUse(sql); ok {
				st.SetDatabase(db)
			}
			if err := withBackend(ctx, cfg.Pool, st, &sticky, func(b pool.Backend) error {
				return execCommand(front, b, data, false)
			}); err != nil {
				return err
			}
		case mysql.ComStmtPrepare:
			// Statement IDs are connection-local → pin backend.
			st.MarkReserved()
			if err := withBackend(ctx, cfg.Pool, st, &sticky, func(b pool.Backend) error {
				return execCommand(front, b, data, true)
			}); err != nil {
				return err
			}
		case mysql.ComStmtExecute, mysql.ComStmtReset:
			if err := withBackend(ctx, cfg.Pool, st, &sticky, func(b pool.Backend) error {
				return execCommand(front, b, data, false)
			}); err != nil {
				return err
			}
		case mysql.ComStmtClose:
			// COM_STMT_CLOSE has no response packet from the server.
			if err := withBackend(ctx, cfg.Pool, st, &sticky, func(b pool.Backend) error {
				return writeBackend(b, data)
			}); err != nil {
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

func withBackend(ctx context.Context, p *pool.Pool, st *session.State, sticky *pool.Backend, fn func(pool.Backend) error) error {
	var b pool.Backend
	var err error
	release := false
	switch {
	case st.NeedsSticky() && *sticky != nil:
		b = *sticky
	case st.NeedsSticky():
		b, err = p.Acquire(ctx)
		if err != nil {
			return err
		}
		*sticky = b
	default:
		b, err = p.Acquire(ctx)
		if err != nil {
			return err
		}
		release = true
	}
	ferr := fn(b)
	if release {
		_ = p.Release(b)
	}
	return ferr
}

func writeBackend(b pool.Backend, cmd []byte) error {
	mb, ok := b.(*pool.MySQLBackend)
	if !ok {
		return fmt.Errorf("proxy: backend type %T not supported", b)
	}
	back := mb.Packet()
	back.ResetSequence()
	buf := make([]byte, 4+len(cmd))
	copy(buf[4:], cmd)
	return back.WritePacket(buf)
}

func execCommand(front *packet.Conn, b pool.Backend, cmd []byte, prepare bool) error {
	mb, ok := b.(*pool.MySQLBackend)
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
	// Backend handshake negotiates CLIENT_DEPRECATE_EOF with MySQL 8.
	const deprecateEOF = true
	if prepare {
		return mysql.ForwardPrepareResponse(front, back, deprecateEOF)
	}
	return mysql.ForwardResponse(front, back, deprecateEOF)
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
