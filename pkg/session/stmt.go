package session

import (
	"fmt"

	"github.com/pingcap/tidb/pkg/parser/ast"
)

// Stmt is one frontend prepared statement and its per-slice backend ids.
type Stmt struct {
	ID      uint32
	SQL     string
	AST     ast.StmtNode
	Params  uint16
	Targets map[string]uint32 // slice name -> backend statement id
}

// StmtTable is the per-session prepared statement map. The command loop is
// single-threaded per session; no lock is needed.
type StmtTable struct {
	stmts map[uint32]*Stmt
}

// NewStmtTable returns an empty statement table.
func NewStmtTable() *StmtTable {
	return &StmtTable{stmts: make(map[uint32]*Stmt)}
}

// Add inserts s; duplicate ids are an error.
func (t *StmtTable) Add(s *Stmt) error {
	if s == nil {
		return fmt.Errorf("session: nil stmt")
	}
	if _, ok := t.stmts[s.ID]; ok {
		return fmt.Errorf("session: duplicate stmt id %d", s.ID)
	}
	t.stmts[s.ID] = s
	return nil
}

// Get returns the statement with id.
func (t *StmtTable) Get(id uint32) (*Stmt, bool) {
	s, ok := t.stmts[id]
	return s, ok
}

// Delete removes id and reports whether it existed.
func (t *StmtTable) Delete(id uint32) bool {
	if _, ok := t.stmts[id]; !ok {
		return false
	}
	delete(t.stmts, id)
	return true
}

// Len returns the number of open statements.
func (t *StmtTable) Len() int {
	return len(t.stmts)
}

// Reset removes every statement (COM_RESET_CONNECTION).
func (t *StmtTable) Reset() {
	clear(t.stmts)
}
