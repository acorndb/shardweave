// Package execute implements the single data-plane execution pipeline.
package execute

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"

	"github.com/acorndb/shardweave/pkg/backend"
	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/result"
	"github.com/acorndb/shardweave/pkg/route"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
)

// Runtime is per frontend-connection mutable state.
type Runtime struct {
	Front  *packet.Conn
	State  *session.State
	leases map[string]*lease // slice name -> lease
}

type lease struct {
	slice *backend.Slice
	conn  backend.Conn
	pool  *backend.Pool
	// reusable is false when the statement may have changed backend session
	// state we cannot reproduce, or when the response was not fully consumed.
	// Such connections are discarded instead of returned to the pool.
	reusable bool
}

// Executor is the process-shared execution strategy.
type Executor interface {
	Handle(ctx context.Context, rt *Runtime, stmt *sql.Statement) error
	Release(rt *Runtime, discard bool) error
	Close() error
}

// Pipeline is the single execution implementation shared by all modes.
type Pipeline struct {
	cluster  *backend.Cluster
	builder  plan.Builder
	sharding bool
}

// New builds a Pipeline for a runtime cluster and a mode-specific planner.
func New(c *backend.Cluster, builder plan.Builder) (*Pipeline, error) {
	if c == nil || len(c.Names()) == 0 {
		return nil, fmt.Errorf("execute: requires cluster")
	}
	if builder.Router == nil {
		return nil, fmt.Errorf("execute: requires router")
	}
	return &Pipeline{cluster: c, builder: builder, sharding: builder.Sharding}, nil
}

// Close closes the underlying cluster pools.
func (p *Pipeline) Close() error {
	if p == nil || p.cluster == nil {
		return nil
	}
	return p.cluster.Close()
}

func (p *Pipeline) releaseOne(rt *Runtime, sliceName string, discard bool) error {
	if rt == nil || rt.leases == nil {
		return nil
	}
	l := rt.leases[sliceName]
	if l == nil {
		return nil
	}
	delete(rt.leases, sliceName)
	// A connection that is broken, dirty, or still mid-transaction must not go
	// back to the pool; closing it lets the pool rebuild a clean one.
	if discard || !l.reusable || rt.State.NeedsSticky() {
		return l.slice.Discard(l.pool, l.conn)
	}
	return l.slice.Release(l.pool, l.conn)
}

// Release returns or discards every lease held by the session.
func (p *Pipeline) Release(rt *Runtime, discard bool) error {
	if p == nil || p.cluster == nil || rt == nil || len(rt.leases) == 0 {
		return nil
	}
	var first error
	for name := range rt.leases {
		if err := p.releaseOne(rt, name, discard); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// acquireLease reuses the session lease for the slice or acquires a new one.
func (p *Pipeline) acquireLease(ctx context.Context, rt *Runtime, target route.Target) (*lease, bool, error) {
	if rt.leases == nil {
		rt.leases = make(map[string]*lease)
	}
	if l := rt.leases[target.Slice]; l != nil {
		return l, false, nil
	}
	slice, err := p.cluster.Get(target.Slice)
	if err != nil {
		return nil, false, err
	}
	conn, pool, err := slice.Acquire(ctx, target.Role)
	if err != nil {
		return nil, false, err
	}
	l := &lease{slice: slice, conn: conn, pool: pool, reusable: true}
	rt.leases[target.Slice] = l
	return l, true, nil
}

// execTarget acquires/reuses a lease, aligns it and executes the command. It
// returns the backend stream; callers own forwarding/decoding and cleanup.
func (p *Pipeline) execTarget(ctx context.Context, rt *Runtime, stmt *sql.Statement, target route.Target) (result.Stream, error) {
	l, acquired, err := p.acquireLease(ctx, rt, target)
	if err != nil {
		return nil, err
	}
	stream, err := p.execOnLease(ctx, rt, stmt, target, l)
	if err != nil {
		l.reusable = false
		if acquired {
			_ = p.releaseOne(rt, target.Slice, false)
		}
		return nil, err
	}
	return stream, nil
}

// execOnLease aligns and executes on an already-acquired lease. Dispatch uses
// it to run independent shard targets concurrently.
func (p *Pipeline) execOnLease(ctx context.Context, rt *Runtime, stmt *sql.Statement, target route.Target, l *lease) (result.Stream, error) {
	if dirtyForReuse(stmt.EnsureClass()) {
		l.reusable = false
	}
	if err := l.conn.Align(ctx, backend.AlignSettings{
		Database: targetDatabase(rt, target),
		Charset:  rt.State.Charset(),
		Vars:     rt.State.Vars(),
	}); err != nil {
		return nil, err
	}
	payload, err := p.payloadFor(rt, stmt, target)
	if err != nil {
		return nil, err
	}
	return l.conn.Exec(ctx, backend.ExecRequest{
		Payload: payload,
		Prepare: stmt.Cmd == mysql.ComStmtPrepare,
		NoReply: stmt.Cmd == mysql.ComStmtClose,
	})
}

func targetDatabase(rt *Runtime, target route.Target) string {
	if target.DB != "" {
		return target.DB
	}
	return rt.State.Database()
}

// Handle builds a Plan, acquires/reuses a lease, executes and forwards the result.
func (p *Pipeline) Handle(ctx context.Context, rt *Runtime, stmt *sql.Statement) error {
	if p == nil || p.cluster == nil {
		return fmt.Errorf("execute: nil pipeline")
	}
	if rt == nil || rt.State == nil {
		return fmt.Errorf("execute: nil runtime state")
	}
	if stmt == nil {
		return fmt.Errorf("execute: nil statement")
	}

	if stmt.Kind == sql.KindExplainRoute {
		rt.State.SetRowCount(-1)
		return p.explainRoute(ctx, rt, stmt)
	}

	rewriteSessionFunc(rt.State, stmt)
	success := false
	defer func() {
		if !success {
			markRowCountError(rt.State, stmt)
		}
	}()

	pl, err := p.builder.Build(ctx, rt.State, stmt)
	if err != nil {
		return err
	}
	if pl.Kind == plan.Reject {
		return pl.Reject
	}
	success, err = p.executePlan(ctx, rt, stmt, pl)
	return err
}

// frontendDeprecateEOF matches CLIENT_DEPRECATE_EOF advertised by the frontend
// handshake. Keep this in sync if capability negotiation becomes per-session.
const frontendDeprecateEOF = true

// executePlan dispatches one plan. N=1 keeps the raw fast path; N>1 is the
// sharding scatter path (concat merge).
func (p *Pipeline) executePlan(ctx context.Context, rt *Runtime, stmt *sql.Statement, pl *plan.Plan) (bool, error) {
	switch len(pl.Targets) {
	case 1:
		return p.runOne(ctx, rt, stmt, pl.Targets[0])
	default:
		return p.dispatch(ctx, rt, stmt, pl)
	}
}

// dirtyForReuse reports whether a statement may have left backend session
// state that the proxy cannot reproduce on acquire. SET NAMES, USE,
// autocommit and whitelisted session variables are tracked by Effect/Align;
// everything else forces a discard. Non-whitelisted SET SESSION never reaches
// this layer (rejected in Class.UnsupportedReason).
func dirtyForReuse(c *sql.Class) bool {
	if c == nil {
		return false
	}
	return c.ScanFailed || c.HasExecComment || c.Multi || c.TempTable ||
		c.LockTables || c.UserVarWrite
}

// streamDrained reports whether a backend stream was consumed to its
// terminator. Streams that do not implement Drained are assumed complete
// (test doubles and synthetic streams).
type drainReporter interface{ Drained() bool }

func streamDrained(s result.Stream) bool {
	if d, ok := s.(drainReporter); ok {
		return d.Drained()
	}
	return true
}

func (p *Pipeline) markUnreusable(rt *Runtime, sliceName string) {
	if rt == nil || rt.leases == nil {
		return
	}
	if l := rt.leases[sliceName]; l != nil {
		l.reusable = false
	}
}

// runOne executes one target, acquires/reuses its lease and forwards the raw
// response. It returns whether session state was successfully updated.
func (p *Pipeline) runOne(ctx context.Context, rt *Runtime, stmt *sql.Statement, target route.Target) (ok bool, err error) {
	// Evaluate stickiness after execution: BEGIN sets InTx from the backend status.
	defer func() {
		if !rt.State.NeedsSticky() {
			_ = p.Release(rt, err != nil)
		}
	}()

	stream, err := p.execTarget(ctx, rt, stmt, target)
	if err != nil {
		return false, err
	}
	defer stream.Close()

	if p.sharding && stmt.Cmd == mysql.ComStmtPrepare {
		pf, ok := stream.(prepareForwarder)
		if !ok {
			return false, fmt.Errorf("execute: stream %T cannot forward prepare", stream)
		}
		clientID := rt.State.NextStmtID()
		info, backendID, err := pf.ForwardPrepareTo(rt.Front, clientID)
		if err != nil {
			p.markUnreusable(rt, target.Slice)
			return false, err
		}
		if !streamDrained(stream) {
			p.markUnreusable(rt, target.Slice)
		}
		if info.ServerErr {
			return false, nil
		}
		entry := &session.Stmt{
			ID:      clientID,
			Slice:   target.Slice,
			SQL:     stmt.Raw,
			AST:     stmt.AST,
			Targets: map[string]uint32{target.Slice: backendID},
		}
		if err := rt.State.Stmts().Add(entry); err != nil {
			return false, err
		}
		rt.State.MarkStmtPinned() // keep the backend connection holding the stmt
		return true, nil
	}

	raw, ok := stream.(rawForwarder)
	if !ok {
		return false, fmt.Errorf("execute: stream %T not forwardable", stream)
	}
	info, err := raw.ForwardTo(ctx, rt.Front)
	if err != nil {
		p.markUnreusable(rt, target.Slice)
		return false, err
	}
	if !streamDrained(stream) {
		p.markUnreusable(rt, target.Slice)
	}
	if info.ServerErr {
		return false, nil // ERR already forwarded; rowCount is marked -1
	}
	if stmt.Cmd != mysql.ComStmtPrepare && stmt.Cmd != mysql.ComStmtClose {
		rt.State.SetInTx(info.Status&mysql.ServerStatusInTrans != 0)
	}
	applyEffect(rt.State, stmt.Effect)
	applyResponseState(rt.State, stmt, info)
	if p.sharding && stmt.Cmd == mysql.ComStmtClose {
		rt.State.Stmts().Delete(stmt.StmtID)
		if rt.State.Stmts().Len() == 0 {
			rt.State.UnmarkStmtPinned()
		}
	}
	return true, nil
}

// payloadFor uses the per-target rewritten SQL when present, and rewrites the
// proxy-owned statement id to the backend id for COM_STMT_* commands.
func (p *Pipeline) payloadFor(rt *Runtime, stmt *sql.Statement, target route.Target) ([]byte, error) {
	switch stmt.Cmd {
	case mysql.ComStmtExecute, mysql.ComStmtReset, mysql.ComStmtClose:
		if !p.sharding {
			return stmt.Payload, nil
		}
		entry, ok := rt.State.Stmts().Get(stmt.StmtID)
		if !ok {
			return nil, fmt.Errorf("execute: unknown prepared statement %d", stmt.StmtID)
		}
		backendID := entry.Targets[target.Slice]
		if backendID == 0 {
			return nil, fmt.Errorf("execute: statement %d has no backend id on %s", stmt.StmtID, target.Slice)
		}
		if len(stmt.Payload) < 5 {
			return nil, fmt.Errorf("execute: short COM_STMT payload")
		}
		payload := append([]byte(nil), stmt.Payload...)
		binary.LittleEndian.PutUint32(payload[1:5], backendID)
		return payload, nil
	case mysql.ComQuery:
		if target.SQL != "" {
			payload := make([]byte, 1+len(target.SQL))
			payload[0] = mysql.ComQuery
			copy(payload[1:], target.SQL)
			return payload, nil
		}
	}
	return stmt.Payload, nil
}

// explainRoute builds a plan without executing it and writes a synthetic
// result set describing targets or the rejection reason.
func (p *Pipeline) explainRoute(ctx context.Context, rt *Runtime, stmt *sql.Statement) error {
	var (
		pl       *plan.Plan
		buildErr error
	)
	if stmt.Err != nil {
		buildErr = stmt.Err
	} else {
		pl, buildErr = p.builder.Build(ctx, rt.State, stmt)
	}
	cols, rows := explainRows(stmt, pl, buildErr)
	return writeTextResult(rt.Front, cols, rows)
}

func explainRows(stmt *sql.Statement, pl *plan.Plan, buildErr error) ([]string, [][]string) {
	cols := []string{"kind", "slice", "role", "db", "sql", "reason"}
	if buildErr != nil {
		var rej *errs.MySQL
		if errors.As(buildErr, &rej) {
			return cols, [][]string{{"Reject", "", "", "", "", rej.Message}}
		}
		return cols, [][]string{{"Error", "", "", "", "", buildErr.Error()}}
	}
	if pl == nil {
		return cols, [][]string{{"Error", "", "", "", "", "nil plan"}}
	}
	if pl.Kind == plan.Reject {
		reason := ""
		if pl.Reject != nil {
			reason = pl.Reject.Message
		}
		return cols, [][]string{{"Reject", "", "", "", "", reason}}
	}
	rows := make([][]string, 0, len(pl.Targets))
	for _, t := range pl.Targets {
		sqlText := t.SQL
		if sqlText == "" {
			sqlText = stmt.Raw
		}
		rows = append(rows, []string{
			pl.Kind.String(), t.Slice, string(t.Role), t.DB, sqlText, "",
		})
	}
	return cols, rows
}

func writeTextResult(front *packet.Conn, cols []string, rows [][]string) error {
	if front == nil {
		return fmt.Errorf("execute: nil frontend for explain")
	}
	columns := make([]mysql.Column, 0, len(cols))
	for _, name := range cols {
		columns = append(columns, mysql.Column{
			Name: name, Charset: 0x2d, ColumnLength: 1024, Type: 0xfd, // VAR_STRING
		})
	}
	if err := mysql.WriteResultHeader(front, columns); err != nil {
		return err
	}
	for _, row := range rows {
		cells := make([][]byte, len(row))
		for i, v := range row {
			cells[i] = []byte(v)
		}
		if err := writeFrontPayload(front, mysql.EncodeTextRow(cells)); err != nil {
			return err
		}
	}
	// The frontend handshake advertises CLIENT_DEPRECATE_EOF; keep this in sync
	// if capability negotiation becomes per-session.
	return mysql.WriteEOF(front, frontendDeprecateEOF, mysql.ServerStatusAutocommit, 0)
}

func writeFrontPayload(front *packet.Conn, payload []byte) error {
	buf := make([]byte, 4+len(payload))
	copy(buf[4:], payload)
	return front.WritePacket(buf)
}

// rewriteSessionFunc answers SELECT LAST_INSERT_ID()/ROW_COUNT() from session
// state. The rewritten literal is sent to a backend so the proxy does not need
// to synthesize a result set; the value is independent of the backend data.
func rewriteSessionFunc(st *session.State, stmt *sql.Statement) {
	if stmt.Cmd != mysql.ComQuery {
		return
	}
	fn := stmt.EnsureClass().SessionFunc
	if fn == nil {
		return
	}
	var value string
	switch fn.Kind {
	case sql.SessionFuncLastInsertID:
		value = strconv.FormatUint(st.LastInsertID(), 10)
	case sql.SessionFuncRowCount:
		value = strconv.FormatInt(st.RowCount(), 10)
	}
	rewritten := fn.Rewrite(value)
	payload := make([]byte, 1+len(rewritten))
	payload[0] = mysql.ComQuery
	copy(payload[1:], rewritten)
	stmt.Payload = payload
	stmt.Raw = rewritten
}

// applyResponseState mirrors MySQL's connection-scoped function semantics:
// LAST_INSERT_ID persists across statements (only a non-zero OK value updates
// it); ROW_COUNT is -1 for result sets/errors, affected_rows for DML, and 0 for
// other OK results.
func applyResponseState(st *session.State, stmt *sql.Statement, info mysql.ResponseInfo) {
	if info.LastInsertID != 0 {
		st.SetLastInsertID(info.LastInsertID)
	}
	switch stmt.Cmd {
	case mysql.ComQuery, mysql.ComStmtExecute:
	default:
		return
	}
	if info.ResultSet {
		st.SetRowCount(-1)
		return
	}
	if stmt.Cmd == mysql.ComStmtExecute || stmt.EnsureClass().IsDML() {
		st.SetRowCount(int64(info.AffectedRows))
		return
	}
	st.SetRowCount(0)
}

func markRowCountError(st *session.State, stmt *sql.Statement) {
	if stmt == nil {
		return
	}
	switch stmt.Cmd {
	case mysql.ComQuery, mysql.ComStmtExecute:
		st.SetRowCount(-1)
	}
}

// applyEffect applies a statement's session effect only after a successful
// backend response. Autocommit must be applied before the deferred sticky check.
func applyEffect(st *session.State, e sql.Effect) {
	if e.UseDB != "" {
		st.SetDatabase(e.UseDB)
	}
	if e.Charset != "" {
		st.SetCharset(e.Charset)
	}
	if e.Autocommit != nil {
		st.SetAutocommit(*e.Autocommit)
	}
	for name, value := range e.Vars {
		st.SetVar(name, value)
	}
}
