// Package sql defines the statement shape shared by the proxy, routing, and planning.
package sql

import (
	"strings"

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
}

// IsDML reports whether raw starts with a DML keyword. ROW_COUNT() uses
// affected_rows for these statements and 0/-1 for everything else.
func IsDML(raw string) bool {
	s := trimLeadingSQLComments(strings.TrimSpace(raw))
	i := 0
	for i < len(s) && isSQLIdentChar(s[i]) {
		i++
	}
	switch strings.ToUpper(s[:i]) {
	case "INSERT", "UPDATE", "DELETE", "REPLACE", "LOAD":
		return true
	default:
		return false
	}
}
