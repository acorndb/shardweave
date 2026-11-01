package sql

import (
	"fmt"
	"strings"
)

// SessionFuncKind identifies a connection-scoped function the proxy answers
// from session state instead of the backend.
type SessionFuncKind uint8

const (
	SessionFuncLastInsertID SessionFuncKind = iota
	SessionFuncRowCount
)

// SessionFunc is a parsed `SELECT LAST_INSERT_ID()` / `SELECT ROW_COUNT()`
// statement with an optional alias. Detection lives in the token layer
// (Class.SessionFunc); this type only owns the rewrite.
type SessionFunc struct {
	Kind  SessionFuncKind
	Alias string
	Expr  string // original expression text, used as the column name
}

// ColumnName is the column label MySQL would return for the expression.
func (f SessionFunc) ColumnName() string {
	if f.Alias != "" {
		return f.Alias
	}
	return f.Expr
}

// Rewrite returns an equivalent SELECT with the session value as a literal.
// value must be a preformatted integer.
func (f SessionFunc) Rewrite(value string) string {
	cast := "UNSIGNED"
	if f.Kind == SessionFuncRowCount {
		cast = "SIGNED"
	}
	name := strings.ReplaceAll(f.ColumnName(), "`", "``")
	return fmt.Sprintf("SELECT CAST(%s AS %s) AS `%s`", value, cast, name)
}
