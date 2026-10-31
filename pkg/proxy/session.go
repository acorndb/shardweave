package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/execute"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/acorndb/shardweave/pkg/mysql/server"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
	sqlparser "github.com/acorndb/shardweave/pkg/sql/parser"
)

// SessionConfig wires frontend auth and backend executor for passthrough.
type SessionConfig struct {
	Users            server.UserProvider
	Exec             execute.Executor
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	MaxAllowedPacket int
	// ParseAST enables one TiDB-parser pass per COM_QUERY. It is off for
	// single/rwsplit and on for sharding (routing/rewrite need the AST).
	ParseAST bool
	// Sharding disables legacy sticky behaviors that do not apply when
	// prepared statements are rejected by the shard router.
	Sharding bool
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
			if errors.Is(err, errs.ErrPacketTooLarge) {
				_ = mysql.WriteERR(front, errs.PacketTooLarge())
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
				raw := string(data[1:])
				stmt.Kind = sql.KindQuery
				if inner, ok := sql.ParseExplainRoute(raw); ok {
					stmt.Kind = sql.KindExplainRoute
					raw = inner
					stmt.Payload = append([]byte{mysql.ComQuery}, []byte(raw)...)
				}
				stmt.Raw = raw
				stmt.Effect = sql.ParseSessionEffect(raw)
				if cfg.ParseAST {
					parseStatementAST(stmt, raw)
				}
			case mysql.ComInitDB:
				stmt.Raw = string(data[1:])
				stmt.Kind = sql.KindUse
				stmt.Effect = sql.Effect{UseDB: stmt.Raw}
			case mysql.ComStmtPrepare:
				stmt.Raw = string(data[1:])
				if cfg.ParseAST {
					parseStatementAST(stmt, stmt.Raw)
				}
			case mysql.ComStmtExecute, mysql.ComStmtReset, mysql.ComStmtClose:
				if len(data) >= 5 {
					stmt.StmtID = binary.LittleEndian.Uint32(data[1:5])
				}
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
				if herr := handleExecError(front, err); herr != nil {
					return herr
				}
				continue
			}
		case mysql.ComQuery:
			if session.DetectReservedSQL(stmt.Raw) {
				st.MarkReserved()
			}
			if err := cfg.Exec.Handle(ctx, ec, stmt); err != nil {
				if herr := handleExecError(front, err); herr != nil {
					return herr
				}
				continue
			}
		case mysql.ComStmtPrepare:
			if !cfg.Sharding {
				st.MarkReserved()
			}
			if err := cfg.Exec.Handle(ctx, ec, stmt); err != nil {
				if herr := handleExecError(front, err); herr != nil {
					return herr
				}
				continue
			}
		case mysql.ComStmtExecute, mysql.ComStmtReset, mysql.ComStmtClose:
			if err := cfg.Exec.Handle(ctx, ec, stmt); err != nil {
				if herr := handleExecError(front, err); herr != nil {
					return herr
				}
				continue
			}
		case mysql.ComResetConnection:
			// Match prior behavior: Release failure still clears proxy state.
			_ = cfg.Exec.Release(ec, false)
			st.Reset()
			if err := writeOK(front); err != nil {
				return err
			}
		default:
			_ = mysql.WriteERR(front, errs.UnknownCommand(data[0]))
		}
	}
}

func parseStatementAST(stmt *sql.Statement, raw string) {
	res, err := sqlparser.Parse(raw)
	if err != nil {
		stmt.Err = err
		return
	}
	if len(res.Stmts) == 1 {
		stmt.AST = res.Stmts[0]
	}
}

func handleExecError(front *packet.Conn, err error) error {
	var me *errs.MySQL
	if errors.As(err, &me) {
		if werr := mysql.WriteERR(front, me); werr != nil {
			return werr
		}
		return nil // rejected statement must not kill the session
	}
	if errors.Is(err, errs.ErrPoolExhausted) {
		_ = mysql.WriteERR(front, errs.TooManyConnections())
	}
	return err
}

func writeOK(front *packet.Conn) error {
	p := mysql.EncodeOK(0, 0, mysql.ServerStatusAutocommit, 0)
	buf := make([]byte, 4+len(p))
	copy(buf[4:], p)
	return front.WritePacket(buf)
}
