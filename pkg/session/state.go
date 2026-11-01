package session

// State holds frontend session fields that affect backend connection reuse.
type State struct {
	database   string
	inTx       bool
	autocommit bool
	reserved   bool
	stmtPinned bool
	charset    string

	// Connection-scoped information functions. They are answered by the proxy
	// from these fields, not by the backend connection currently in use.
	lastInsertID uint64
	rowCount     int64
	nextStmtID   uint32

	// vars holds replayable session variables (statement-classification §5).
	// Align re-applies them on every backend connection the session uses.
	vars map[string]string

	stmts *StmtTable
}

// NewState returns a session state with MySQL-default autocommit=ON.
// ROW_COUNT() is -1 until a statement sets it.
func NewState() *State {
	return &State{autocommit: true, rowCount: -1, stmts: NewStmtTable()}
}

func (s *State) Database() string { return s.database }
func (s *State) SetDatabase(db string) {
	s.database = db
}

func (s *State) InTx() bool { return s.inTx }
func (s *State) SetInTx(v bool) {
	s.inTx = v
}

// Autocommit reports SESSION autocommit (default true after handshake).
func (s *State) Autocommit() bool { return s.autocommit }

// SetAutocommit updates autocommit. Setting true clears InTx (kingshard/Gaea handleSetAutoCommit).
func (s *State) SetAutocommit(v bool) {
	s.autocommit = v
	if v {
		s.inTx = false
	}
}

// IsInTransaction matches kingshard/Gaea: IN_TRANS || !autocommit.
func (s *State) IsInTransaction() bool {
	return s.inTx || !s.autocommit
}

func (s *State) Reserved() bool { return s.reserved }
func (s *State) MarkReserved() {
	s.reserved = true
}

// MarkStmtPinned pins the backend connection because the session holds
// prepared statements on it. Unlike reserved, the pin is released when the
// last statement closes (COM_STMT_CLOSE).
func (s *State) MarkStmtPinned() { s.stmtPinned = true }

// UnmarkStmtPinned releases the prepared-statement pin.
func (s *State) UnmarkStmtPinned() { s.stmtPinned = false }

func (s *State) Charset() string { return s.charset }
func (s *State) SetCharset(cs string) {
	s.charset = cs
}

// LastInsertID is the value LAST_INSERT_ID() would return for this session.
func (s *State) LastInsertID() uint64 { return s.lastInsertID }

// SetLastInsertID updates the session value. Callers should only apply a
// non-zero OK-packet last_insert_id: zero means "unchanged" in MySQL.
func (s *State) SetLastInsertID(id uint64) {
	s.lastInsertID = id
}

// RowCount is the value ROW_COUNT() would return for this session.
func (s *State) RowCount() int64 { return s.rowCount }

// SetRowCount updates the session value (-1 for SELECT/error, affected rows
// for DML, 0 for DDL/SET/transaction statements).
func (s *State) SetRowCount(n int64) {
	s.rowCount = n
}

// SetVar records one replayable session variable.
func (s *State) SetVar(name, value string) {
	if s.vars == nil {
		s.vars = make(map[string]string)
	}
	s.vars[name] = value
}

// Vars returns the replayable session variables (do not mutate).
func (s *State) Vars() map[string]string {
	return s.vars
}

// Stmts returns the session prepared statement table.
func (s *State) Stmts() *StmtTable {
	if s.stmts == nil {
		s.stmts = NewStmtTable()
	}
	return s.stmts
}

// NextStmtID returns a proxy-owned statement id. It never returns 0.
func (s *State) NextStmtID() uint32 {
	s.nextStmtID++
	if s.nextStmtID == 0 {
		s.nextStmtID = 1
	}
	return s.nextStmtID
}

// Reset clears transaction/reserved/charset state for COM_RESET_CONNECTION.
// Current database is preserved (MySQL COM_RESET_CONNECTION semantics).
func (s *State) Reset() {
	s.inTx = false
	s.autocommit = true
	s.reserved = false
	s.stmtPinned = false
	s.charset = ""
	s.lastInsertID = 0
	s.rowCount = -1
	s.nextStmtID = 0
	s.vars = nil
	s.Stmts().Reset()
}

// NeedsSticky reports whether the session must keep the same backend connection.
// Includes !autocommit (design §5.9 / kingshard isInTransaction) and the
// prepared-statement pin.
func (s *State) NeedsSticky() bool {
	return s.IsInTransaction() || s.reserved || s.stmtPinned
}
