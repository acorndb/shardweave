package session

import (
	"strings"
	"unicode"
)

// State holds frontend session fields that affect backend connection reuse.
type State struct {
	database string
	inTx     bool
	reserved bool
	charset  string
}

// NewState returns an empty session state.
func NewState() *State { return &State{} }

func (s *State) Database() string { return s.database }
func (s *State) SetDatabase(db string) {
	s.database = db
}

func (s *State) InTx() bool { return s.inTx }
func (s *State) SetInTx(v bool) {
	s.inTx = v
}

func (s *State) Reserved() bool { return s.reserved }
func (s *State) MarkReserved() {
	s.reserved = true
}

func (s *State) Charset() string { return s.charset }
func (s *State) SetCharset(cs string) {
	s.charset = cs
}

// NeedsSticky reports whether the session must keep the same backend connection.
func (s *State) NeedsSticky() bool {
	return s.inTx || s.reserved
}

// DetectReservedSQL heuristically flags statements that pin a backend connection
// (D3-A: user vars / temporary tables / locks). Conservative substring checks for P1.
func DetectReservedSQL(sql string) bool {
	s := stripLeadingComments(strings.TrimSpace(sql))
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
