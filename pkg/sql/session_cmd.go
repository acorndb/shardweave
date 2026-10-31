package sql

import "strings"

// Effect is the session-state change implied by a statement.
// It is applied only after the backend response is known to be OK.
type Effect struct {
	UseDB      string
	Charset    string
	Autocommit *bool
}

// ParseSessionEffect recognizes USE / SET NAMES / SET autocommit in a
// COM_QUERY text. Other statements return the zero Effect.
func ParseSessionEffect(raw string) Effect {
	if db, ok := parseUse(raw); ok {
		return Effect{UseDB: db}
	}
	if cs, ok := parseSetNames(raw); ok {
		return Effect{Charset: cs}
	}
	if v, ok := parseSetAutocommit(raw); ok {
		return Effect{Autocommit: &v}
	}
	return Effect{}
}

func parseUse(sql string) (string, bool) {
	s := strings.TrimSpace(sql)
	if len(s) < 4 {
		return "", false
	}
	if !strings.EqualFold(s[:3], "USE") {
		return "", false
	}
	rest := strings.TrimSpace(s[3:])
	rest = strings.TrimSuffix(rest, ";")
	rest = strings.TrimSpace(rest)
	quoteChars := string([]byte{'`', '"', '\''})
	rest = strings.Trim(rest, quoteChars)
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false
	}
	return rest, true
}

func parseSetNames(sql string) (string, bool) {
	s := strings.TrimSpace(sql)
	s = strings.TrimSuffix(s, ";")
	s = strings.TrimSpace(s)
	upper := strings.ToUpper(s)
	const prefix = "SET NAMES"
	if !strings.HasPrefix(upper, prefix) {
		return "", false
	}
	rest := strings.TrimSpace(s[len(prefix):])
	if rest == "" {
		return "", false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", false
	}
	cs := strings.Trim(fields[0], "`\"'")
	if cs == "" {
		return "", false
	}
	return cs, true
}

// parseSetAutocommit recognizes SET autocommit = 0|1|ON|OFF (design §5.9 / kingshard).
func parseSetAutocommit(sql string) (bool, bool) {
	s := strings.TrimSpace(sql)
	s = strings.TrimSuffix(s, ";")
	s = strings.TrimSpace(s)
	upper := strings.ToUpper(s)
	if !strings.HasPrefix(upper, "SET") {
		return false, false
	}
	rest := strings.TrimSpace(s[3:])
	if len(rest) >= 7 && strings.EqualFold(rest[:7], "SESSION") {
		rest = strings.TrimSpace(rest[7:])
	}
	eq := strings.IndexByte(rest, '=')
	if eq < 0 {
		return false, false
	}
	name := strings.TrimSpace(rest[:eq])
	name = strings.TrimPrefix(strings.ToLower(name), "@@")
	name = strings.TrimPrefix(name, "session.")
	if !strings.EqualFold(name, "autocommit") {
		return false, false
	}
	val := strings.TrimSpace(rest[eq+1:])
	val = strings.Trim(val, "`\"'")
	switch strings.ToUpper(val) {
	case "1", "ON":
		return true, true
	case "0", "OFF":
		return false, true
	default:
		return false, false
	}
}
