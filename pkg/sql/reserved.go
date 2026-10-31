package sql

import (
	"strings"
	"unicode"
)

// DetectReservedSQL heuristically flags statements that pin a backend
// connection (user vars / temporary tables / locks). Conservative substring
// checks; a future StatementClass can replace it with AST classification.
func DetectReservedSQL(raw string) bool {
	s := stripLeadingComments(strings.TrimSpace(raw))
	if s == "" {
		return false
	}
	upper := strings.ToUpper(s)
	switch {
	case strings.Contains(upper, "TEMPORARY TABLE"):
		return true
	case strings.Contains(upper, "GET_LOCK(") || strings.Contains(upper, "RELEASE_LOCK("):
		return true
	case strings.HasPrefix(upper, "LOCK TABLES") || strings.HasPrefix(upper, "UNLOCK TABLES"):
		return true
	case hasUserVariableAssign(s):
		return true
	default:
		return false
	}
}

func hasUserVariableAssign(sql string) bool {
	// SET @x ... or SELECT @x:= ...
	for i := 0; i < len(sql); i++ {
		if sql[i] != '@' {
			continue
		}
		if i+1 >= len(sql) {
			continue
		}
		if sql[i+1] == '@' {
			// Skip the "@@" system-variable pair: it is not connection-scoped.
			i++
			continue
		}
		r := rune(sql[i+1])
		if unicode.IsLetter(r) || r == '_' {
			return true
		}
	}
	return false
}

func stripLeadingComments(sql string) string {
	for {
		sql = strings.TrimLeftFunc(sql, unicode.IsSpace)
		switch {
		case strings.HasPrefix(sql, "--"):
			if i := strings.IndexByte(sql, '\n'); i >= 0 {
				sql = sql[i+1:]
				continue
			}
			return ""
		case strings.HasPrefix(sql, "#"):
			if i := strings.IndexByte(sql, '\n'); i >= 0 {
				sql = sql[i+1:]
				continue
			}
			return ""
		case strings.HasPrefix(sql, "/*"):
			if i := strings.Index(sql, "*/"); i >= 0 {
				sql = sql[i+2:]
				continue
			}
			return sql
		default:
			return sql
		}
	}
}
