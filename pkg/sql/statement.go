// Package sql defines the statement shape shared by the proxy, routing, and planning.
package sql

import (
	"github.com/pingcap/tidb/pkg/parser/ast"
)

// Kind is a coarse statement category used by routing and planning.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindQuery
	KindDML
	KindDDL
	KindSet
	KindUse
	KindShow
	KindExplainRoute
)

// Statement is one frontend command.
//
// Payload is the exact command packet (including the command byte) so the
// execution layer can forward it without re-encoding. Raw is the SQL text for
// COM_QUERY / COM_INIT_DB; AST is filled only when a later stage parses it.
type Statement struct {
	Cmd     byte
	Payload []byte
	Raw     string
	StmtID  uint32 // COM_STMT_* client statement id (proxy-owned)
	AST     ast.StmtNode
	Kind    Kind
	Effect  Effect
	Err     error

	// Class is the single statement-level semantic snapshot. It is computed
	// once from Raw (see EnsureClass) and must be the only input to routing,
	// planning and session decisions.
	Class Class

	classReady bool
}

// EnsureClass returns the semantic snapshot, computing it from Raw on first
// use. A scan failure degrades conservatively (design D2): the statement is
// never treated as a read and the session is pinned to the master.
func (s *Statement) EnsureClass() *Class {
	if s == nil {
		return &Class{}
	}
	if s.classReady {
		return &s.Class
	}
	toks, err := Scan(s.Raw)
	if err != nil {
		s.Class = Class{ScanFailed: true}
	} else {
		s.Class = Classify(s.Raw, toks)
	}
	s.classReady = true
	return &s.Class
}

// ResetClass drops the cached snapshot so the next EnsureClass recomputes it.
// Used after a rewrite replaces Raw (e.g. SELECT LAST_INSERT_ID() → CAST
// literal): judgments must see the rewritten statement, not the original.
func (s *Statement) ResetClass() {
	if s == nil {
		return
	}
	s.Class = Class{}
	s.classReady = false
}
