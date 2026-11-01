package sql

import "strings"

// Effect is the session-state change implied by a statement. It is applied
// only after the backend response is known to be OK.
type Effect struct {
	UseDB      string
	Charset    string
	Autocommit *bool
	// Vars holds replayable session variables (name → value). The proxy
	// re-applies them on every backend connection used by the session.
	Vars map[string]string
}

// StmtKind is the coarse statement category produced by the token layer.
// It is the single statement-level truth; routing/planning read this snapshot
// instead of re-scanning raw SQL (docs/design/statement-classification.md).
type StmtKind uint8

const (
	StmtUnknown StmtKind = iota
	StmtSelect
	StmtInsert
	StmtUpdate
	StmtDelete
	StmtReplace
	StmtDDL
	StmtShow
	StmtSet
	StmtUse
	StmtExplain
	StmtTx
	StmtCall
	StmtWith
	StmtLoad
	StmtOther
)

func (k StmtKind) String() string {
	switch k {
	case StmtSelect:
		return "Select"
	case StmtInsert:
		return "Insert"
	case StmtUpdate:
		return "Update"
	case StmtDelete:
		return "Delete"
	case StmtReplace:
		return "Replace"
	case StmtDDL:
		return "DDL"
	case StmtShow:
		return "Show"
	case StmtSet:
		return "Set"
	case StmtUse:
		return "Use"
	case StmtExplain:
		return "Explain"
	case StmtTx:
		return "Tx"
	case StmtCall:
		return "Call"
	case StmtWith:
		return "With"
	case StmtLoad:
		return "Load"
	case StmtOther:
		return "Other"
	default:
		return "Unknown"
	}
}

// Class is the semantic snapshot of one statement, computed once from the
// token stream. Downstream layers must consume it instead of scanning raw SQL.
type Class struct {
	Kind       StmtKind
	First      string // uppercased first keyword ("" for empty/comment-only input)
	Multi      bool   // a top-level semicolon with tokens after it
	MasterHint bool   // canonical /*master*/ comment (D3)
	// HasExecComment marks MySQL executable comments (/*! ... */). They carry
	// real SQL, so consumers must treat the statement conservatively.
	HasExecComment bool
	LockingRead    bool
	// UserVar is set for any user-variable reference (@x read or write).
	// UserVarWrite is narrower: assignment through := or SET @x =.
	UserVar      bool
	UserVarWrite bool
	// SessionVarWrite / GlobalVarWrite list assigned system variable names
	// (lowercased, scope prefix stripped). They drive the D1 whitelist policy.
	SessionVarWrite []string
	GlobalVarWrite  []string
	TempTable       bool // CREATE TEMPORARY TABLE
	LockTables      bool // LOCK TABLES / UNLOCK TABLES
	// Calls lists function names followed by '(' (stopwords such as IN excluded).
	Calls            []string
	SetNames         string            // SET NAMES <charset>
	UseDB            string            // USE <db>
	Autocommit       *bool             // SET autocommit = 0/1/ON/OFF/TRUE/FALSE
	SessionVars      map[string]string // replayable session variables (name → value)
	ReadOnlyProbe    bool              // probes @@read_only or SHOW ... read_only
	SQLCalcFoundRows bool
	ShowSub          string       // raw (uppercased) text after SHOW, for subtype checks
	SessionFunc      *SessionFunc // exact SELECT LAST_INSERT_ID()/ROW_COUNT()
	// ScanFailed marks lexical failure. Consumers degrade conservatively:
	// never slave, and the session is pinned (design D2).
	ScanFailed bool
}

// Classify builds the semantic snapshot from raw and its token stream.
// raw is needed for faithful labels (expression text) and SHOW subtype checks.
func Classify(raw string, tokens []Token) Class {
	c := Class{}
	sig := make([]Token, 0, len(tokens))
	for _, t := range tokens {
		switch t.Kind {
		case TokComment:
			if strings.EqualFold(strings.TrimSpace(t.Text), "master") {
				c.MasterHint = true
			}
		case TokExecComment:
			c.HasExecComment = true
		default:
			sig = append(sig, t)
		}
	}
	if len(sig) == 0 {
		return c
	}

	c.First = sig[0].Upper
	c.Kind = stmtKind(c.First, sig[0].Kind)
	c.Multi = hasTopLevelSemicolon(sig)
	c.LockingRead = hasLockingRead(sig)
	c.TempTable = hasAdjacentIDents(sig, "TEMPORARY", "TABLE")
	c.LockTables = hasLockTables(sig)
	c.SQLCalcFoundRows = hasIdent(sig, "SQL_CALC_FOUND_ROWS")
	c.UserVar, c.UserVarWrite = scanUserVars(sig)
	c.Calls = scanCalls(sig)

	switch c.Kind {
	case StmtShow:
		c.ShowSub = showSub(raw, sig)
		c.ReadOnlyProbe = containsFold(sig, "read_only")
	case StmtSelect:
		c.ReadOnlyProbe = hasSystemVarContaining(sig, "read_only")
	default:
		c.ReadOnlyProbe = hasSystemVarContaining(sig, "read_only")
	}

	if c.Kind == StmtUse && len(sig) >= 2 {
		if v, ok := aliasText(sig[1]); ok {
			c.UseDB = v
		}
	}
	if c.Kind == StmtSet {
		c.SetNames, c.SessionVarWrite, c.GlobalVarWrite, c.Autocommit, c.SessionVars = parseSet(raw, sig)
	}
	if c.Kind == StmtSelect {
		c.SessionFunc = parseSessionFunc(raw, sig)
	}
	return c
}

// WantSlave reports read/write split intent (layer B only; session layer A is
// applied by the caller). Non-canonical master hints no longer apply (D3).
func (c Class) WantSlave() bool {
	switch c.Kind {
	case StmtSelect, StmtShow, StmtExplain, StmtUse:
	default:
		return false
	}
	if c.Multi || c.MasterHint || c.LockingRead || c.ReadOnlyProbe || c.HasExecComment {
		return false
	}
	return true
}

// Reserved reports whether the statement must pin the session to one backend
// connection. Parity with the previous DetectReservedSQL (track A); session
// variable writes are a Track B extension and are not included here.
func (c Class) Reserved() bool {
	if c.ScanFailed || c.TempTable || c.LockTables || c.UserVar {
		return true
	}
	return c.hasCall("GET_LOCK") || c.hasCall("RELEASE_LOCK")
}

// IsDML reports whether ROW_COUNT() should use affected_rows. LOAD is included
// because LOAD DATA reports affected rows.
func (c Class) IsDML() bool {
	switch c.Kind {
	case StmtInsert, StmtUpdate, StmtDelete, StmtReplace, StmtLoad:
		return true
	default:
		return false
	}
}

// UnsupportedReason returns a stable rejection reason, or "" when the statement
// is supported. Exact rewritable session functions are not rejected: the
// pipeline rewrites them before planning, and Class.SessionFunc marks them.
func (c Class) UnsupportedReason() string {
	if c.Kind == StmtShow {
		if r := showUnsupported(c.ShowSub); r != "" {
			return r
		}
	}
	if c.Kind == StmtSet {
		if c.SetNames != "" && !strings.EqualFold(c.SetNames, "utf8mb4") {
			return "SET NAMES " + c.SetNames + " is not supported by shardweave; only utf8mb4 is supported"
		}
		for _, v := range c.SessionVarWrite {
			if !sessionVarAllowed(v) {
				return "SET SESSION " + v + " is not supported by shardweave"
			}
		}
		for _, v := range c.GlobalVarWrite {
			if strings.EqualFold(v, "sql_mode") {
				return "SET sql_mode is not supported by shardweave"
			}
		}
	}
	if c.SQLCalcFoundRows {
		return "SQL_CALC_FOUND_ROWS is not supported by shardweave"
	}
	for _, f := range c.Calls {
		switch f {
		case "FOUND_ROWS":
			return "FOUND_ROWS() is not supported by shardweave"
		case "LAST_INSERT_ID":
			if c.SessionFunc == nil {
				return "LAST_INSERT_ID() is not supported in this form by shardweave"
			}
		case "ROW_COUNT":
			if c.SessionFunc == nil {
				return "ROW_COUNT() is not supported in this form by shardweave"
			}
		}
	}
	return ""
}

// Effect is the session-state change implied by the statement. It is applied
// only after a successful backend response.
func (c Class) Effect() Effect {
	var e Effect
	if c.UseDB != "" {
		e.UseDB = c.UseDB
	}
	if c.SetNames != "" {
		e.Charset = c.SetNames
	}
	if c.Autocommit != nil {
		e.Autocommit = c.Autocommit
	}
	if len(c.SessionVars) > 0 {
		e.Vars = c.SessionVars
	}
	return e
}

func (c Class) hasCall(name string) bool {
	for _, f := range c.Calls {
		if f == name {
			return true
		}
	}
	return false
}

// --- classification helpers -------------------------------------------------

func stmtKind(first string, kind TokenKind) StmtKind {
	if kind != TokIdent {
		return StmtUnknown
	}
	switch first {
	case "SELECT":
		return StmtSelect
	case "INSERT":
		return StmtInsert
	case "UPDATE":
		return StmtUpdate
	case "DELETE":
		return StmtDelete
	case "REPLACE":
		return StmtReplace
	case "SHOW":
		return StmtShow
	case "SET":
		return StmtSet
	case "USE":
		return StmtUse
	case "EXPLAIN", "DESC", "DESCRIBE":
		return StmtExplain
	case "BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT", "XA", "START":
		return StmtTx
	case "CALL":
		return StmtCall
	case "WITH":
		return StmtWith
	case "LOAD":
		return StmtLoad
	case "CREATE", "DROP", "ALTER", "RENAME", "TRUNCATE", "GRANT", "REVOKE",
		"FLUSH", "ANALYZE", "OPTIMIZE", "REPAIR", "CHECK":
		return StmtDDL
	default:
		return StmtOther
	}
}

func isPunct(t Token, text string) bool {
	return t.Kind == TokPunct && t.Text == text
}

func hasTopLevelSemicolon(sig []Token) bool {
	for i, t := range sig {
		if isPunct(t, ";") && i+1 < len(sig) {
			return true
		}
	}
	return false
}

func hasLockingRead(sig []Token) bool {
	for i := 0; i+1 < len(sig); i++ {
		if sig[i].Kind != TokIdent {
			continue
		}
		switch sig[i].Upper {
		case "FOR":
			if next := sig[i+1].Upper; next == "UPDATE" || next == "SHARE" {
				return true
			}
		case "LOCK":
			if i+3 < len(sig) && sig[i+1].Upper == "IN" &&
				sig[i+2].Upper == "SHARE" && sig[i+3].Upper == "MODE" {
				return true
			}
		}
	}
	return false
}

func hasAdjacentIDents(sig []Token, first, second string) bool {
	for i := 0; i+1 < len(sig); i++ {
		if sig[i].Kind == TokIdent && sig[i].Upper == first &&
			sig[i+1].Kind == TokIdent && sig[i+1].Upper == second {
			return true
		}
	}
	return false
}

func hasLockTables(sig []Token) bool {
	if len(sig) < 2 {
		return false
	}
	if sig[0].Kind != TokIdent || sig[1].Kind != TokIdent || sig[1].Upper != "TABLES" {
		return false
	}
	return sig[0].Upper == "LOCK" || sig[0].Upper == "UNLOCK"
}

func hasIdent(sig []Token, upper string) bool {
	for _, t := range sig {
		if t.Kind == TokIdent && t.Upper == upper {
			return true
		}
	}
	return false
}

func containsFold(sig []Token, sub string) bool {
	sub = strings.ToLower(sub)
	for _, t := range sig {
		if strings.Contains(strings.ToLower(t.Text), sub) {
			return true
		}
	}
	return false
}

func hasSystemVarContaining(sig []Token, sub string) bool {
	sub = strings.ToLower(sub)
	for _, t := range sig {
		if t.Kind == TokSystemVar && strings.Contains(strings.ToLower(t.Text), sub) {
			return true
		}
	}
	return false
}

func scanUserVars(sig []Token) (userVar, userVarWrite bool) {
	for i, t := range sig {
		if t.Kind != TokUserVar {
			continue
		}
		userVar = true
		if i+1 < len(sig) && sig[i+1].Kind == TokOp && sig[i+1].Text == ":=" {
			userVarWrite = true
		}
	}
	if !userVarWrite {
		// SET @x = 1 assigns a user variable with plain '='.
		if len(sig) > 0 && sig[0].Kind == TokIdent && sig[0].Upper == "SET" {
			for _, seg := range splitTopLevelTokens(sig[1:]) {
				if len(seg) > 0 && seg[0].Kind == TokUserVar && findAssignOp(seg) > 0 {
					userVarWrite = true
				}
			}
		}
	}
	return userVar, userVarWrite
}

// nonFuncKeywords are words that may be followed by '(' without being calls.
var nonFuncKeywords = map[string]bool{
	"IN": true, "EXISTS": true, "NOT": true, "AND": true, "OR": true, "ON": true,
	"USING": true, "VALUES": true, "SELECT": true, "WHERE": true, "HAVING": true,
	"BETWEEN": true, "LIKE": true, "INTERVAL": true, "CASE": true, "WHEN": true,
	"THEN": true, "ELSE": true, "END": true, "JOIN": true, "FROM": true, "AS": true,
	"OVER": true, "PARTITION": true, "ORDER": true, "GROUP": true, "BY": true,
	"LIMIT": true, "UNION": true, "ALL": true, "DISTINCT": true, "IS": true,
	"NULL": true, "SET": true, "INTO": true, "FOR": true, "LOCK": true,
	"TABLE": true, "TABLES": true, "CREATE": true, "UPDATE": true, "DELETE": true,
	"INSERT": true, "REPLACE": true, "SHOW": true, "USE": true, "EXPLAIN": true,
	"DESC": true, "DESCRIBE": true, "WITH": true, "CALL": true, "LOAD": true,
	"START": true, "BEGIN": true, "COMMIT": true, "ROLLBACK": true,
	"SAVEPOINT": true, "XA": true, "KEY": true, "PRIMARY": true, "UNIQUE": true,
	"REFERENCES": true, "CHECK": true, "DEFAULT": true, "INDEX": true,
}

func scanCalls(sig []Token) []string {
	var out []string
	for i := 0; i+1 < len(sig); i++ {
		if sig[i].Kind != TokIdent || nonFuncKeywords[sig[i].Upper] {
			continue
		}
		if !isPunct(sig[i+1], "(") {
			continue
		}
		out = append(out, sig[i].Upper)
	}
	return out
}

// splitTopLevelTokens splits a token slice on top-level ','/'}' separators.
func splitTopLevelTokens(sig []Token) [][]Token {
	var out [][]Token
	depth := 0
	start := 0
	for i, t := range sig {
		if t.Kind != TokPunct {
			continue
		}
		switch t.Text {
		case "(":
			depth++
		case ")":
			if depth > 0 {
				depth--
			}
		case ",", ";":
			if depth == 0 {
				if i > start {
					out = append(out, sig[start:i])
				}
				start = i + 1
			}
		}
	}
	if start < len(sig) {
		out = append(out, sig[start:])
	}
	return out
}

// findAssignOp returns the index of a top-level '='/':=' operator, or -1.
func findAssignOp(seg []Token) int {
	depth := 0
	for i, t := range seg {
		switch {
		case t.Kind == TokPunct && t.Text == "(":
			depth++
		case t.Kind == TokPunct && t.Text == ")":
			if depth > 0 {
				depth--
			}
		case depth == 0 && t.Kind == TokOp && (t.Text == "=" || t.Text == ":="):
			return i
		}
	}
	return -1
}

// sessionVarWhitelist lists session variables the proxy can replay on backend
// connections (statement-classification design §5, decision D1). autocommit is
// handled separately via Effect.Autocommit and transaction stickiness.
var sessionVarWhitelist = map[string]bool{
	"time_zone":             true,
	"transaction_isolation": true,
}

// sessionVarAllowed reports whether a session variable may be assigned at all.
func sessionVarAllowed(name string) bool {
	return name == "autocommit" || sessionVarWhitelist[name]
}

// parseSet extracts SET NAMES, system-variable writes, autocommit and the
// values of replayable session variables.
func parseSet(raw string, sig []Token) (setNames string, session, global []string, autocommit *bool, vars map[string]string) {
	if len(sig) >= 3 && sig[0].Upper == "SET" && sig[1].Upper == "NAMES" {
		if v, ok := aliasText(sig[2]); ok {
			setNames = v
		}
	}
	for _, seg := range splitTopLevelTokens(sig[1:]) {
		op := findAssignOp(seg)
		if op <= 0 {
			continue
		}
		name, scope, ok := assignmentTarget(seg[:op])
		if !ok {
			continue
		}
		if scope == "global" {
			global = append(global, name)
		} else {
			session = append(session, name)
		}
		if name == "autocommit" {
			if v, ok := boolValue(seg[op+1:]); ok {
				autocommit = v
			}
			continue
		}
		if scope != "global" && sessionVarWhitelist[name] {
			if v, ok := valueText(raw, seg[op+1:]); ok && !strings.EqualFold(v, "DEFAULT") {
				if vars == nil {
					vars = make(map[string]string)
				}
				vars[name] = v
			}
		}
	}
	return setNames, session, global, autocommit, vars
}

// valueText renders one assignment value: a single string literal loses its
// quotes, anything else is the raw source slice between the first and last
// token (so multi-word values like READ COMMITTED survive verbatim).
func valueText(raw string, toks []Token) (string, bool) {
	if len(toks) == 0 {
		return "", false
	}
	if len(toks) == 1 && toks[0].Kind == TokString {
		return toks[0].Text, true
	}
	last := toks[len(toks)-1]
	if toks[0].Pos < 0 || last.End > len(raw) {
		return "", false
	}
	return raw[toks[0].Pos:last.End], true
}

// assignmentTarget normalizes the left-hand side of a SET assignment.
// User variables are excluded: they are session state tracked separately.
func assignmentTarget(lhs []Token) (name, scope string, ok bool) {
	if len(lhs) == 0 {
		return "", "", false
	}
	if len(lhs) == 1 {
		switch lhs[0].Kind {
		case TokUserVar:
			return "", "", false
		case TokSystemVar:
			low := strings.ToLower(lhs[0].Text)
			switch {
			case strings.HasPrefix(low, "session."):
				return low[len("session."):], "session", true
			case strings.HasPrefix(low, "local."):
				return low[len("local."):], "session", true
			case strings.HasPrefix(low, "global."):
				return low[len("global."):], "global", true
			default:
				return low, "session", true
			}
		case TokQuotedIdent, TokIdent:
			return strings.ToLower(lhs[0].Text), "session", true
		}
		return "", "", false
	}
	if lhs[0].Kind == TokIdent {
		switch lhs[0].Upper {
		case "SESSION", "LOCAL":
			if n, ok := aliasText(lhs[1]); ok {
				return strings.ToLower(n), "session", true
			}
		case "GLOBAL":
			if n, ok := aliasText(lhs[1]); ok {
				return strings.ToLower(n), "global", true
			}
		}
	}
	return "", "", false
}

func boolValue(tokens []Token) (*bool, bool) {
	if len(tokens) == 0 {
		return nil, false
	}
	t := tokens[0]
	switch t.Kind {
	case TokNumber:
		switch t.Text {
		case "0":
			v := false
			return &v, true
		case "1":
			v := true
			return &v, true
		}
	case TokIdent:
		switch t.Upper {
		case "ON", "TRUE":
			v := true
			return &v, true
		case "OFF", "FALSE":
			v := false
			return &v, true
		}
	}
	return nil, false
}

func showSub(raw string, sig []Token) string {
	if len(sig) < 2 {
		return ""
	}
	end := sig[len(sig)-1].End
	if end > len(raw) {
		end = len(raw)
	}
	return strings.ToUpper(raw[sig[1].Pos:end])
}

func showUnsupported(rest string) string {
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

// parseSessionFunc recognizes exactly `SELECT <func>() [AS alias]`.
// The expression text is sliced from raw so the column label matches MySQL.
func parseSessionFunc(raw string, sig []Token) *SessionFunc {
	sig = trimTrailingSemicolons(sig)
	if len(sig) < 4 {
		return nil
	}
	if sig[0].Kind != TokIdent || sig[0].Upper != "SELECT" {
		return nil
	}
	if sig[1].Kind != TokIdent {
		return nil
	}
	var kind SessionFuncKind
	switch sig[1].Upper {
	case "LAST_INSERT_ID":
		kind = SessionFuncLastInsertID
	case "ROW_COUNT":
		kind = SessionFuncRowCount
	default:
		return nil
	}
	if !isPunct(sig[2], "(") || !isPunct(sig[3], ")") {
		return nil
	}
	end := sig[3].End
	if end > len(raw) {
		return nil
	}
	fn := &SessionFunc{Kind: kind, Expr: raw[sig[1].Pos:end]}

	rest := sig[4:]
	switch len(rest) {
	case 0:
		return fn
	case 1:
		if rest[0].Kind == TokIdent && rest[0].Upper == "AS" {
			return nil
		}
		alias, ok := aliasText(rest[0])
		if !ok {
			return nil
		}
		fn.Alias = alias
		return fn
	case 2:
		if rest[0].Kind != TokIdent || rest[0].Upper != "AS" {
			return nil
		}
		alias, ok := aliasText(rest[1])
		if !ok {
			return nil
		}
		fn.Alias = alias
		return fn
	default:
		return nil
	}
}

func trimTrailingSemicolons(sig []Token) []Token {
	for len(sig) > 0 && isPunct(sig[len(sig)-1], ";") {
		sig = sig[:len(sig)-1]
	}
	return sig
}

// aliasText returns the identifier/literal text for an alias position.
func aliasText(t Token) (string, bool) {
	switch t.Kind {
	case TokIdent, TokQuotedIdent, TokString:
		if t.Text == "" {
			return "", false
		}
		return t.Text, true
	default:
		return "", false
	}
}

// hasWordPrefix reports whether s starts with prefix as a whole word.
// '(' counts as a word boundary so "COUNT(*)" matches "COUNT(*)...".
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

// trimLeadingSQLComments removes leading --, # and block comments. It stays
// for callers that intentionally work on raw text (EXPLAIN ROUTE); semantic
// decisions use Class instead.
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
