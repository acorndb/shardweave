// Package source implements read/write source selection rules (design §5).
// Rules are ported from kingshard/Gaea/dbpack; see classify_test.go refs.
package source

import (
	"strings"
	"unicode"
)

// ClassifyWantSlave reports Layer-B intent (ignore session). false → master.
// ref: Gaea checkExecuteFromSlave; kingshard handleSelect/handleExec; design §5.3.
func ClassifyWantSlave(sql string) bool {
	sql = stripLeadingComments(strings.TrimSpace(sql))
	if sql == "" {
		return false // M9
	}
	if hasMasterHint(sql) {
		return false // M7
	}
	if isMultiStatement(sql) {
		return false // §5.10.1
	}

	kw, rest := firstKeyword(sql)
	switch kw {
	case "INSERT", "UPDATE", "DELETE", "REPLACE", "TRUNCATE", "LOAD",
		"CREATE", "DROP", "ALTER", "RENAME", "GRANT", "REVOKE", "FLUSH",
		"ANALYZE", "OPTIMIZE", "REPAIR", "CHECK",
		"BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT", "XA", "SET":
		return false // M1–M4
	case "START":
		if nextKeyword(rest) == "TRANSACTION" {
			return false // M3
		}
		return false
	case "USE":
		return true // M5 / kingshard prefer slave when not in tx (Layer A applies later)
	case "WITH":
		return false // §5.12 CTE conservative
	case "SELECT":
		if isLockingRead(sql) || isReadOnlyVarProbe(sql) {
			return false // M6 M8
		}
		return true // S1
	case "SHOW":
		if isReadOnlyVarProbe(sql) {
			return false // M8
		}
		return true // S2
	case "EXPLAIN", "DESCRIBE", "DESC":
		return true // S3
	default:
		return false // M9
	}
}

func hasMasterHint(sql string) bool {
	lower := strings.ToLower(sql)
	if strings.Contains(lower, "/*master*/") || strings.Contains(lower, "/*+ master */") {
		return true
	}
	// Gaea token *master*
	return strings.Contains(lower, "*master*")
}

func isLockingRead(sql string) bool {
	u := strings.ToUpper(sql)
	return strings.Contains(u, "FOR UPDATE") ||
		strings.Contains(u, "FOR SHARE") ||
		strings.Contains(u, "LOCK IN SHARE MODE")
}

func isReadOnlyVarProbe(sql string) bool {
	lower := strings.ToLower(sql)
	if !strings.Contains(lower, "read_only") {
		return false
	}
	// SHOW ... read_only or @@read_only / @@global.read_only
	return strings.Contains(lower, "@@") || strings.HasPrefix(strings.TrimSpace(lower), "show")
}

func isMultiStatement(sql string) bool {
	// naive: unquoted semicolon — false positives → master (acceptable)
	inStr := false
	var quote rune
	for i, r := range sql {
		if inStr {
			if r == quote && (i == 0 || sql[i-1] != '\\') {
				inStr = false
			}
			continue
		}
		if r == '\'' || r == '"' || r == '`' {
			inStr = true
			quote = r
			continue
		}
		if r == ';' {
			rest := strings.TrimSpace(sql[i+1:])
			return rest != ""
		}
	}
	return false
}

func firstKeyword(sql string) (kw, rest string) {
	i := 0
	for i < len(sql) && unicode.IsSpace(rune(sql[i])) {
		i++
	}
	start := i
	for i < len(sql) {
		r := rune(sql[i])
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			i++
			continue
		}
		break
	}
	if start == i {
		return "", sql
	}
	return strings.ToUpper(sql[start:i]), sql[i:]
}

func nextKeyword(rest string) string {
	kw, _ := firstKeyword(rest)
	return kw
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
			// keep /*master*/ for hint detector — only strip if not master hint
			end := strings.Index(sql, "*/")
			if end < 0 {
				return sql
			}
			block := strings.ToLower(sql[:end+2])
			if strings.Contains(block, "master") {
				return sql // leave for hasMasterHint
			}
			sql = sql[end+2:]
			continue
		default:
			return sql
		}
	}
}
