package proxy

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/acorndb/prism/pkg/executor"
	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/mysql/packet"
	"github.com/acorndb/prism/pkg/mysql/server"
	"github.com/acorndb/prism/pkg/session"
	"github.com/acorndb/prism/pkg/sql"
)

// SessionConfig wires frontend auth and backend executor for passthrough.
type SessionConfig struct {
	Users server.UserProvider
	Exec  executor.Executor
}

// Serve runs one frontend connection: handshake then command loop.
func Serve(ctx context.Context, raw net.Conn, cfg SessionConfig) error {
	if cfg.Exec == nil {
		return fmt.Errorf("proxy: nil executor")
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

	ec := &executor.Conn{Front: front, State: st}
	defer func() {
		_ = cfg.Exec.ReleaseSticky(ec, true)
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
		stmt := &sql.Statement{Cmd: cmd, Payload: data}
		if len(data) > 1 {
			switch cmd {
			case mysql.ComQuery:
				stmt.Raw = string(data[1:])
				stmt.Kind = sql.KindQuery
			case mysql.ComInitDB:
				stmt.Raw = string(data[1:])
				stmt.Kind = sql.KindUse
			case mysql.ComStmtPrepare:
				stmt.Raw = string(data[1:])
			}
		}

		switch cmd {
		case mysql.ComQuit:
			return nil
		case mysql.ComPing:
			if err := writeOK(front); err != nil {
				return err
			}
		case mysql.ComInitDB:
			st.SetDatabase(string(data[1:]))
			if err := cfg.Exec.Handle(ctx, ec, stmt); err != nil {
				return err
			}
		case mysql.ComQuery:
			if session.DetectReservedSQL(stmt.Raw) {
				st.MarkReserved()
			}
			if db, ok := parseUse(stmt.Raw); ok {
				st.SetDatabase(db)
			}
			if cs, ok := parseSetNames(stmt.Raw); ok {
				st.SetCharset(cs)
			}
			if v, ok := parseSetAutocommit(stmt.Raw); ok {
				st.SetAutocommit(v)
			}
			if err := cfg.Exec.Handle(ctx, ec, stmt); err != nil {
				return err
			}
		case mysql.ComStmtPrepare:
			st.MarkReserved()
			if err := cfg.Exec.Handle(ctx, ec, stmt); err != nil {
				return err
			}
		case mysql.ComStmtExecute, mysql.ComStmtReset, mysql.ComStmtClose:
			if err := cfg.Exec.Handle(ctx, ec, stmt); err != nil {
				return err
			}
		case mysql.ComResetConnection:
			// Match prior behavior: Release failure still clears proxy state.
			_ = cfg.Exec.ReleaseSticky(ec, false)
			st.Reset()
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
