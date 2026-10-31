package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/acorndb/shardweave/pkg/execute"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/acorndb/shardweave/pkg/mysql/server"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
)

// SessionConfig wires frontend auth and backend executor for passthrough.
type SessionConfig struct {
	Users            server.UserProvider
	Exec             execute.Executor
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	MaxAllowedPacket int
}

// Serve runs one frontend connection: handshake then command loop.
func Serve(ctx context.Context, raw net.Conn, cfg SessionConfig) error {
	if cfg.Exec == nil {
		return fmt.Errorf("proxy: nil executor")
	}
	sc, err := server.NewConnWithOptions(raw, cfg.Users, server.Options{
		ReadTimeout:      cfg.ReadTimeout,
		WriteTimeout:     cfg.WriteTimeout,
		MaxAllowedPacket: cfg.MaxAllowedPacket,
	})
	if err != nil {
		return err
	}
	front := sc.PacketConn()
	st := session.NewState()
	if db := sc.Database(); db != "" {
		st.SetDatabase(db)
	}

	ec := &execute.Runtime{Front: front, State: st}
	defer func() {
		_ = cfg.Exec.Release(ec, true)
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
			if errors.Is(err, packet.ErrPacketTooLarge) {
				_ = writeERR(front, 1153, "08S01", "Got a packet bigger than 'max_allowed_packet' bytes")
			}
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
				stmt.Effect = sql.ParseSessionEffect(stmt.Raw)
			case mysql.ComInitDB:
				stmt.Raw = string(data[1:])
				stmt.Kind = sql.KindUse
				stmt.Effect = sql.Effect{UseDB: stmt.Raw}
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
			if err := cfg.Exec.Handle(ctx, ec, stmt); err != nil {
				return handleExecError(front, err)
			}
		case mysql.ComQuery:
			if session.DetectReservedSQL(stmt.Raw) {
				st.MarkReserved()
			}
			if err := cfg.Exec.Handle(ctx, ec, stmt); err != nil {
				return handleExecError(front, err)
			}
		case mysql.ComStmtPrepare:
			st.MarkReserved()
			if err := cfg.Exec.Handle(ctx, ec, stmt); err != nil {
				return handleExecError(front, err)
			}
		case mysql.ComStmtExecute, mysql.ComStmtReset, mysql.ComStmtClose:
			if err := cfg.Exec.Handle(ctx, ec, stmt); err != nil {
				return handleExecError(front, err)
			}
		case mysql.ComResetConnection:
			// Match prior behavior: Release failure still clears proxy state.
			_ = cfg.Exec.Release(ec, false)
			st.Reset()
			if err := writeOK(front); err != nil {
				return err
			}
		default:
			_ = writeERR(front, 1047, "08S01", fmt.Sprintf("command %d not supported yet", data[0]))
		}
	}
}

func handleExecError(front *packet.Conn, err error) error {
	if errors.Is(err, execute.ErrPoolExhausted) {
		_ = writeERR(front, 1040, "08004", "Too many connections")
	}
	return err
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
