// Package sql defines the statement shape shared by the proxy, routing, and planning.
package sql

import "github.com/pingcap/tidb/pkg/parser/ast"

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
	AST     ast.StmtNode
	Kind    Kind
	Effect  Effect
	Err     error
}
