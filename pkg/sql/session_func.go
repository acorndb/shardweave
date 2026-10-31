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
// statement with an optional alias.
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

// ParseSessionFunc matches exactly `SELECT <func>() [AS alias]` (optionally
// with a leading comment or trailing semicolon). Anything more complex is left
// to UnsupportedReason and is rejected rather than answered with a wrong value.
func ParseSessionFunc(raw string) (SessionFunc, bool) {
	s := trimLeadingSQLComments(strings.TrimSpace(raw))
	s = strings.TrimSpace(strings.TrimSuffix(s, ";"))
	upper := strings.ToUpper(s)
	if !hasWordPrefix(upper, "SELECT") {
		return SessionFunc{}, false
	}

	i := skipSQLSpaces(s, len("SELECT"))
	start := i
	for i < len(s) && isSQLIdentChar(s[i]) {
		i++
	}
	var kind SessionFuncKind
	switch strings.ToUpper(s[start:i]) {
	case "LAST_INSERT_ID":
		kind = SessionFuncLastInsertID
	case "ROW_COUNT":
		kind = SessionFuncRowCount
	default:
		return SessionFunc{}, false
	}

	i = skipSQLSpaces(s, i)
	if i >= len(s) || s[i] != '(' {
		return SessionFunc{}, false
	}
	i = skipSQLSpaces(s, i+1)
	if i >= len(s) || s[i] != ')' {
		return SessionFunc{}, false
	}
	i++
	expr := s[start:i]

	i = skipSQLSpaces(s, i)
	alias := ""
	if i < len(s) {
		if hasWordPrefix(upper[i:], "AS") {
			i = skipSQLSpaces(s, i+2)
		}
		var ok bool
		alias, i, ok = parseSQLAlias(s, i)
		if !ok {
			return SessionFunc{}, false
		}
		i = skipSQLSpaces(s, i)
	}
	if i != len(s) {
		return SessionFunc{}, false
	}
	return SessionFunc{Kind: kind, Alias: alias, Expr: expr}, true
}

func parseSQLAlias(s string, i int) (string, int, bool) {
	if i >= len(s) {
		return "", i, false
	}
	switch s[i] {
	case '`', '\'', '"':
		quote := s[i]
		i++
		var b strings.Builder
		for i < len(s) {
			switch {
			case s[i] == '\\' && quote != '`' && i+1 < len(s):
				b.WriteByte(s[i+1])
				i += 2
			case s[i] == quote:
				if i+1 < len(s) && s[i+1] == quote {
					b.WriteByte(quote)
					i += 2
					continue
				}
				return b.String(), i + 1, true
			default:
				b.WriteByte(s[i])
				i++
			}
		}
		return "", i, false
	default:
		start := i
		for i < len(s) && isSQLIdentChar(s[i]) {
			i++
		}
		if start == i {
			return "", i, false
		}
		return s[start:i], i, true
	}
}

func skipSQLSpaces(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

func isSQLIdentChar(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z')
}
