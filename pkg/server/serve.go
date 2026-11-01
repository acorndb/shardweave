package server

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
	mysqlserver "github.com/acorndb/shardweave/pkg/mysql/server"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
	sqlparser "github.com/acorndb/shardweave/pkg/sql/parser"
	"github.com/acorndb/shardweave/pkg/util/zerocopy"
)

// SessionConfig wires frontend auth and backend executor for passthrough.
type SessionConfig struct {
	Users            mysqlserver.UserProvider
	Exec             execute.Executor
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	MaxAllowedPacket int
	// Sharding marks the session as running in sharding mode: statements get
	// one TiDB-parser pass (routing/rewrite need the AST) and prepared
	// statements go through the proxy statement table instead of pinning a
	// backend. single/rwsplit keep the zero-parse passthrough path.
	Sharding bool
	// ConnectionID is the frontend connection id advertised in the handshake
	// and answered by CONNECTION_ID(). Zero falls back to 1 (see
	// mysqlserver.Options).
	ConnectionID uint32
}

// rejectTooManyConnections sends ER_CON_COUNT_ERROR as the first packet
// instead of dropping the TCP connection. MySQL does the same once its hard
// connection limit is reached; clients then see 1040 rather than a reset.
func rejectTooManyConnections(conn net.Conn, timeout time.Duration) {
	pc := packet.NewConn(conn)
	if timeout <= 0 {
		timeout = time.Second
	}
	pc.SetWriteTimeout(timeout)
	_ = mysql.WriteERR(pc, errs.TooManyConnections())
	_ = pc.Close()
}

// Serve runs one frontend connection: handshake then command loop.
func Serve(ctx context.Context, raw net.Conn, cfg SessionConfig) error {
	if cfg.Exec == nil {
		return fmt.Errorf("server: nil executor")
	}
	sc, err := mysqlserver.NewConnWithOptions(raw, cfg.Users, mysqlserver.Options{
		ReadTimeout:      cfg.ReadTimeout,
		WriteTimeout:     cfg.WriteTimeout,
		MaxAllowedPacket: cfg.MaxAllowedPacket,
		ConnectionID:     cfg.ConnectionID,
	})
	if err != nil {
		return err
	}
	front := sc.PacketConn()
	st := session.NewState()
	st.SetConnectionID(sc.ConnectionID())
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
				raw := zerocopy.HackString(data[1:])
				stmt.Kind = sql.KindQuery
				if inner, ok := sql.ParseExplainRoute(raw); ok {
					stmt.Kind = sql.KindExplainRoute
					raw = inner
					stmt.Payload = append([]byte{mysql.ComQuery}, []byte(raw)...)
				}
				stmt.Raw = raw
				stmt.Effect = stmt.EnsureClass().Effect()
				if cfg.Sharding {
					parseStatementAST(stmt, raw)
				}
			case mysql.ComInitDB:
				stmt.Raw = zerocopy.HackString(data[1:])
				stmt.Kind = sql.KindUse
				stmt.Effect = sql.Effect{UseDB: stmt.Raw}
			case mysql.ComStmtPrepare:
				stmt.Raw = zerocopy.HackString(data[1:])
				if cfg.Sharding {
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
			status := mysql.ServerStatusWord(st.Autocommit(), st.InTx())
			if err := mysql.WriteOK(front, 0, 0, status, 0); err != nil {
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
			if stmt.EnsureClass().Reserved() {
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
			// Match prior behavior: Release failure still clears the session state.
			_ = cfg.Exec.Release(ec, false)
			st.Reset()
			status := mysql.ServerStatusWord(st.Autocommit(), st.InTx())
			if err := mysql.WriteOK(front, 0, 0, status, 0); err != nil {
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

// handleExecError writes one ERR packet. A MySQL error can arrive after part
// of a result set was already forwarded (sharded merge failure); ERR is a
// valid terminator there, so the session stays alive.
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
