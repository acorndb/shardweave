package sql

import "strings"

// UnsupportedReason reports why a statement is explicitly unsupported by the
// proxy, or "" if it can be routed. These statements are rejected with
// ER_NOT_SUPPORTED_YET (1235) instead of being sent to a backend that cannot
// provide correct semantics for them.
//
// Product decision (2026-10-31): SHOW WARNINGS/ERRORS, replication-status
// SHOW statements, and SET sql_mode are not supported. Other session variables
// are tracked separately by the hardening plan.
func UnsupportedReason(raw string) string {
	s := trimLeadingSQLComments(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	upper := strings.ToUpper(s)

	if hasKeyword(upper, "SHOW") {
		return showUnsupportedReason(strings.TrimSpace(upper[len("SHOW"):]))
	}
	if hasKeyword(upper, "SET") {
		if setAssignsVariable(s[len("SET"):], "sql_mode") {
			return "SET sql_mode is not supported by shardweave"
		}
	}

	code := strings.ToUpper(stripSQLStrings(s))
	switch {
	case containsSQLIdent(code, "SQL_CALC_FOUND_ROWS"):
		return "SQL_CALC_FOUND_ROWS is not supported by shardweave"
	case containsSQLFunc(code, "FOUND_ROWS"):
		return "FOUND_ROWS() is not supported by shardweave"
	case containsSQLFunc(code, "LAST_INSERT_ID"):
		return "LAST_INSERT_ID() is not supported in this form by shardweave"
	case containsSQLFunc(code, "ROW_COUNT"):
		return "ROW_COUNT() is not supported in this form by shardweave"
	}
	return ""
}

func showUnsupportedReason(rest string) string {
	switch {
	case hasWordPrefix(rest, "WARNINGS"):
		return "SHOW WARNINGS is not supported by shardweave"
	case hasWordPrefix(rest, "ERRORS"):
		return "SHOW ERRORS is not supported by shardweave"
	case hasWordPrefix(rest, "COUNT(*)"):
		tail := strings.TrimSpace(rest[len("COUNT(*)"):])
		switch {
		case hasWordPrefix(tail, "WARNINGS"):
			return "SHOW COUNT(*) WARNINGS is not supported by shardweave"
		case hasWordPrefix(tail, "ERRORS"):
			return "SHOW COUNT(*) ERRORS is not supported by shardweave"
		}
	case hasWordPrefix(rest, "MASTER STATUS"):
		return "SHOW MASTER STATUS is not supported by shardweave"
	case hasWordPrefix(rest, "BINARY LOGS"), hasWordPrefix(rest, "BINARY LOG STATUS"),
		hasWordPrefix(rest, "MASTER LOGS"):
		return "SHOW BINARY LOGS is not supported by shardweave"
	case hasWordPrefix(rest, "SLAVE STATUS"):
		return "SHOW SLAVE STATUS is not supported by shardweave"
	case hasWordPrefix(rest, "REPLICA STATUS"):
		return "SHOW REPLICA STATUS is not supported by shardweave"
	}
	return ""
}

// setAssignsVariable reports whether a SET assignment list assigns name.
// It understands scope prefixes, @@ forms, and top-level comma-separated
// assignments. Values are never inspected, so string literals cannot trigger
// a false positive.
func setAssignsVariable(rest, name string) bool {
	for _, part := range splitTopLevel(rest, ',') {
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue // e.g. SET NAMES utf8mb4 / SET TRANSACTION ...
		}
		lhs := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(part[:eq]), ":"))
		fields := strings.Fields(lhs)
		if len(fields) > 0 {
			switch strings.ToUpper(fields[0]) {
			case "SESSION", "GLOBAL", "LOCAL":
				fields = fields[1:]
			}
		}
		lhs = strings.Trim(strings.TrimPrefix(strings.Join(fields, " "), "@@"), "`")
		lhs = strings.ToLower(lhs)
		lhs = strings.TrimPrefix(lhs, "session.")
		lhs = strings.TrimPrefix(lhs, "global.")
		lhs = strings.TrimPrefix(lhs, "local.")
		if lhs == name {
			return true
		}
	}
	return false
}

// splitTopLevel splits on sep outside single/double/backtick quotes.
func splitTopLevel(s string, sep byte) []string {
	var parts []string
	start := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case sep:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

func hasKeyword(s, keyword string) bool {
	return hasWordPrefix(s, keyword)
}

func hasWordPrefix(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	if len(s) == len(prefix) {
		return true
	}
	switch s[len(prefix)] {
	case ' ', '\t', '\n', '\r', ';', '(':
		return true
	default:
		return false
	}
}

// containsSQLFunc reports whether name is used as a function call (followed by
// optional whitespace and an opening parenthesis) in s.
func containsSQLFunc(s, name string) bool {
	for i := 0; i+len(name) <= len(s); i++ {
		if s[i:i+len(name)] != name {
			continue
		}
		if i > 0 && isSQLIdentChar(s[i-1]) {
			continue
		}
		j := i + len(name)
		if j < len(s) && isSQLIdentChar(s[j]) {
			continue
		}
		k := j
		for k < len(s) {
			switch s[k] {
			case ' ', '\t', '\n', '\r':
				k++
				continue
			}
			break
		}
		if k < len(s) && s[k] == '(' {
			return true
		}
	}
	return false
}

func containsSQLIdent(s, name string) bool {
	for i := 0; i+len(name) <= len(s); i++ {
		if s[i:i+len(name)] != name {
			continue
		}
		if i > 0 && isSQLIdentChar(s[i-1]) {
			continue
		}
		j := i + len(name)
		if j < len(s) && isSQLIdentChar(s[j]) {
			continue
		}
		return true
	}
	return false
}

// stripSQLStrings blanks string literals, quoted identifiers and comments so
// keyword detection cannot be fooled by text inside them.
func stripSQLStrings(s string) string {
	b := []byte(s)
	var quote byte
	for i := 0; i < len(b); i++ {
		c := b[i]
		if quote != 0 {
			if c == '\\' && quote != '`' {
				b[i] = ' '
				if i+1 < len(b) {
					b[i+1] = ' '
					i++
				}
				continue
			}
			if c == quote {
				quote = 0
			}
			b[i] = ' '
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
			b[i] = ' '
		case '/':
			if i+1 < len(b) && b[i+1] == '*' {
				b[i], b[i+1] = ' ', ' '
				i += 2
				for i < len(b) {
					if b[i] == '*' && i+1 < len(b) && b[i+1] == '/' {
						b[i], b[i+1] = ' ', ' '
						i++
						break
					}
					b[i] = ' '
					i++
				}
			}
		case '-':
			if i+1 < len(b) && b[i+1] == '-' {
				b[i], b[i+1] = ' ', ' '
				i += 2
				for i < len(b) && b[i] != '\n' {
					b[i] = ' '
					i++
				}
			}
		case '#':
			b[i] = ' '
			for i < len(b) && b[i] != '\n' {
				b[i] = ' '
				i++
			}
		}
	}
	return string(b)
}

// trimLeadingSQLComments removes leading --, # and /* */ comments.
func trimLeadingSQLComments(s string) string {
	for {
		s = strings.TrimSpace(s)
		switch {
		case strings.HasPrefix(s, "--"), strings.HasPrefix(s, "#"):
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return ""
			}
			s = s[i+1:]
		case strings.HasPrefix(s, "/*"):
			i := strings.Index(s, "*/")
			if i < 0 {
				return ""
			}
			s = s[i+2:]
		default:
			return s
		}
	}
}
