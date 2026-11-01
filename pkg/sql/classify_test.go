package sql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// benchInputs is a small realistic mix: read path, write path, session SET,
// comment/hint and a predicate.
var benchInputs = []string{
	"SELECT id, name FROM orders WHERE user_id = 12345 ORDER BY id LIMIT 20",
	"INSERT INTO orders (id, user_id, amount) VALUES (1, 2, 3.5)",
	"UPDATE orders SET amount = 10 WHERE id = 9",
	"SET SESSION sort_buffer_size = 1024",
	"/*master*/ SELECT * FROM orders WHERE status = 'paid'",
	"SELECT @@session.autocommit",
}

// BenchmarkScanClassify measures the single-pass token path (one Scan + one
// Classify per statement).
func BenchmarkScanClassify(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw := benchInputs[i%len(benchInputs)]
		toks, err := Scan(raw)
		if err != nil {
			b.Fatal(err)
		}
		c := Classify(raw, toks)
		_ = c.WantSlave() && c.Reserved() && c.UnsupportedReason() == "" && len(c.Effect().Vars) == 0
	}
}

// BenchmarkLegacyStringScans reproduces the pre-migration cost profile: the
// same statement was scanned independently by ParseSessionEffect,
// UnsupportedReason (including a full stripSQLStrings copy), DetectReservedSQL
// and ClassifyWantSlave. It exists only as the A1 performance fence; no
// production code depends on it.
func BenchmarkLegacyStringScans(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		legacyChain(benchInputs[i%len(benchInputs)])
	}
}

func legacyChain(raw string) {
	s := strings.TrimSpace(raw)
	upper := strings.ToUpper(strings.TrimSuffix(s, ";"))

	// 1. ParseSessionEffect
	_ = strings.HasPrefix(upper, "SET NAMES") || strings.HasPrefix(upper, "SET") ||
		(len(s) >= 3 && strings.EqualFold(s[:3], "USE"))

	// 2. UnsupportedReason: blanks literals/comments into a full copy.
	code := strings.ToUpper(legacyStripSQLStrings(s))
	_ = strings.Contains(code, "SQL_CALC_FOUND_ROWS") ||
		strings.Contains(code, "FOUND_ROWS(") ||
		strings.Contains(code, "LAST_INSERT_ID(") ||
		strings.Contains(code, "ROW_COUNT(")

	// 3. DetectReservedSQL
	_ = strings.Contains(upper, "TEMPORARY TABLE") ||
		strings.Contains(upper, "GET_LOCK(") ||
		strings.Contains(upper, "RELEASE_LOCK(") ||
		strings.HasPrefix(upper, "LOCK TABLES") ||
		strings.Contains(upper, "@")

	// 4. ClassifyWantSlave
	_ = strings.Contains(strings.ToLower(raw), "read_only") ||
		strings.Contains(strings.ToLower(raw), "for update")
}

// legacyStripSQLStrings is a faithful copy of the deleted helper, kept in the
// benchmark to make the comparison honest.
func legacyStripSQLStrings(s string) string {
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

func boolPtr(v bool) *bool { return &v }

func classify(t *testing.T, raw string) Class {
	t.Helper()
	toks, err := Scan(raw)
	require.NoError(t, err, raw)
	return Classify(raw, toks)
}

// TestClassify_WantSlave migrates TestClassify_CorpusB from
// pkg/route/source/classify_test.go. Same cases, same expectations; the
// implementation under test is the token-based Class instead of raw string
// heuristics.
func TestClassify_WantSlave(t *testing.T) {
	cases := []struct {
		id   string
		sql  string
		want bool
		ref  string
	}{
		{"B1", "INSERT INTO t VALUES (1)", false, "kingshard handleExec fromSlave=false"},
		{"B2", "UPDATE t SET a=1", false, "kingshard handleExec"},
		{"B3", "DELETE FROM t", false, "kingshard handleExec"},
		{"B4", "REPLACE INTO t VALUES (1)", false, "kingshard handleExec"},
		{"B5", "TRUNCATE t", false, "kingshard handleExec"},
		{"B6", "CREATE TABLE t(i INT)", false, "Gaea non-Select/Show → master"},
		{"B7", "SET NAMES utf8mb4", false, "M4"},
		{"B8", "SET autocommit=0", false, "M4"},
		{"B9", "USE db1", true, "kingshard handleUseDB fromSlave=true"},
		{"B10", "SELECT * FROM t", true, "kingshard handleSelect default"},
		{"B11", "SELECT * FROM t FOR UPDATE", false, "Gaea checkExecuteFromSlave"},
		{"B12", "SELECT * FROM t LOCK IN SHARE MODE", false, "Gaea checkExecuteFromSlave"},
		{"B13", "SELECT /*master*/ * FROM t", false, "D3 canonical hint"},
		{"B14", "/*master*/ SELECT * FROM t", false, "D3 leading hint"},
		{"B15", "SHOW TABLES", true, "Gaea StmtShow"},
		{"B16", "SHOW VARIABLES LIKE '%read_only%'", false, "Gaea readonlyVariable"},
		{"B17", "SELECT @@read_only", false, "Gaea @@read_only"},
		{"B18", "EXPLAIN SELECT * FROM t", true, "S3"},
		{"B19", "SELECT * FROM t; DELETE FROM t", false, "§5.10.1 multi-stmt"},
		{"B20", "-- c\nSELECT 1", true, "comment strip"},
		{"B21", "select * from t", true, "case"},
		{"B22", "-- only", false, "M9 empty"},
		// D3: non-canonical hint syntaxes no longer route to master.
		{"B23", "SELECT /*+ master */ * FROM t", true, "D3 removed syntax"},
		{"B24", "SELECT *master* FROM t", true, "D3 removed legacy token"},
		// Token layer must not see keywords inside literals/identifiers.
		{"B25", "SELECT '*master*' FROM t", true, "literal is not a hint"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			got := classify(t, tc.sql).WantSlave()
			require.Equal(t, tc.want, got, "%s ref=%s sql=%q", tc.id, tc.ref, tc.sql)
		})
	}
}

// TestClassify_Unsupported migrates TestUnsupportedReason from
// pkg/sql/unsupported_test.go.
func TestClassify_Unsupported(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string // empty means supported
	}{
		{"show_warnings", "SHOW WARNINGS", "SHOW WARNINGS"},
		{"show_warnings_lower", "show warnings", "SHOW WARNINGS"},
		{"show_warnings_comment", "/* hint */ SHOW WARNINGS LIMIT 10", "SHOW WARNINGS"},
		{"show_errors", "SHOW ERRORS", "SHOW ERRORS"},
		{"show_count_warnings", "SHOW COUNT(*) WARNINGS", "WARNINGS"},
		{"show_count_errors", "SHOW COUNT(*) ERRORS", "ERRORS"},
		{"show_master_status", "SHOW MASTER STATUS", "SHOW MASTER STATUS"},
		{"show_binary_logs", "SHOW BINARY LOGS", "BINARY LOGS"},
		{"show_binary_log_status", "SHOW BINARY LOG STATUS", "BINARY LOG"},
		{"show_slave_status", "SHOW SLAVE STATUS", "SLAVE STATUS"},
		{"show_replica_status", "SHOW REPLICA STATUS", "REPLICA STATUS"},
		{"set_sql_mode", "SET sql_mode = 'ANSI_QUOTES'", "sql_mode"},
		{"set_session_sql_mode", "SET SESSION sql_mode = ''", "sql_mode"},
		{"set_at_at_sql_mode", "SET @@SESSION.sql_mode = ''", "sql_mode"},
		{"set_global_sql_mode", "SET GLOBAL sql_mode = ''", "sql_mode"},
		{"set_at_at_global_sql_mode", "SET @@GLOBAL.sql_mode = ''", "sql_mode"},
		{"set_local_sql_mode", "SET LOCAL sql_mode = ''", "sql_mode"},
		{"set_backtick_sql_mode", "SET `sql_mode` = ''", "sql_mode"},
		{"set_sql_mode_assign_colon", "SET sql_mode := ''", "sql_mode"},
		{"set_multi_assignment", "SET autocommit = 1, sql_mode = ''", "sql_mode"},
		{"set_sql_mode_literal", "SET @x = 'sql_mode'", ""},
		{"found_rows", "SELECT FOUND_ROWS()", "FOUND_ROWS"},
		{"calc_found_rows", "SELECT SQL_CALC_FOUND_ROWS id FROM t LIMIT 1", "SQL_CALC_FOUND_ROWS"},
		{"last_insert_id_expression", "SELECT LAST_INSERT_ID() + 1", "LAST_INSERT_ID"},
		{"last_insert_id_where", "SELECT * FROM t WHERE id = LAST_INSERT_ID()", "LAST_INSERT_ID"},
		{"user_var_last_insert_id", "SET @x = LAST_INSERT_ID()", "LAST_INSERT_ID"},
		{"string_literal_function", "SELECT 'LAST_INSERT_ID()'", ""},
		{"comment_function", "SELECT 1 /* ROW_COUNT() */", ""},
		{"rewritten_alias", "SELECT CAST(42 AS UNSIGNED) AS `LAST_INSERT_ID()`", ""},
		{"select", "SELECT 1", ""},
		{"show_tables", "SHOW TABLES", ""},
		{"show_variables", "SHOW VARIABLES LIKE 'sql_mode'", ""},
		{"set_names", "SET NAMES utf8mb4", ""},
		{"set_names_collate", "SET NAMES utf8mb4 COLLATE utf8mb4_bin", ""},
		{"set_names_quoted", "SET NAMES 'utf8mb4'", ""},
		// Decision 2026-11-01: only utf8mb4 connection charsets are accepted;
		// anything else is rejected instead of silently transcoding results.
		{"set_names_latin1", "SET NAMES latin1", "utf8mb4"},
		{"set_names_utf8", "SET NAMES utf8", "utf8mb4"},
		{"set_names_gbk", "SET NAMES gbk", "utf8mb4"},
		{"set_autocommit", "SET autocommit = 0", ""},
		{"set_user_var", "SET @x = 1", ""},
		// The standalone checker rejects every surviving session-function
		// reference: the exact COM_QUERY form is rewritten to a CAST literal
		// (and its Class recomputed) before judgment, so a reference that is
		// still visible here means the form is not rewritable or the command
		// cannot be rewritten (COM_STMT_PREPARE).
		{"row_count_alias", "SELECT ROW_COUNT() AS n", "ROW_COUNT"},
		{"last_insert_id_exact", "SELECT LAST_INSERT_ID()", "LAST_INSERT_ID"},
		{"connection_id_exact", "SELECT CONNECTION_ID()", "CONNECTION_ID"},
		{"connection_id_expression", "SELECT CONNECTION_ID() + 1", "CONNECTION_ID"},
		{"connection_id_rewritten", "SELECT CAST(7 AS UNSIGNED) AS `CONNECTION_ID()`", ""},
		{"kill_connection", "KILL CONNECTION 1", "KILL"},
		{"kill_query", "kill query 1", "KILL"},
		{"kill_in_string", "SELECT 'KILL 1'", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := classify(t, tc.query).UnsupportedReason()
			if tc.want == "" {
				require.Empty(t, reason, tc.query)
				return
			}
			require.Contains(t, reason, tc.want, tc.query)
		})
	}
}

// TestClassify_Reserved migrates TestDetectReservedSQL.
func TestClassify_Reserved(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"SELECT 1", false},
		{"SET @x = 1", true},
		{"set @foo := 2", true},
		{"CREATE TEMPORARY TABLE t (id INT)", true},
		{"create temporary table t(id int)", true},
		{"SELECT GET_LOCK('a', 1)", true},
		{"SELECT RELEASE_LOCK('a')", true},
		{"LOCK TABLES t READ", true},
		{"SET NAMES utf8mb4", false},
		{"SELECT @@server_id", false},
		{"SELECT @@GLOBAL.max_connections", false},
		{"SET @@SESSION.sql_mode = 'ANSI'", false},
		{"SELECT @probe", true},
		// Ledger: keywords inside literals must not trigger detection.
		{"SELECT 'CREATE TEMPORARY TABLE t'", false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			require.Equal(t, tc.want, classify(t, tc.raw).Reserved(), tc.raw)
		})
	}
}

// TestClassify_Effect migrates TestParseSessionEffect and the parseUse /
// parseSetNames / parseSetAutocommit tables.
func TestClassify_Effect(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want Effect
	}{
		{name: "use", sql: "USE app", want: Effect{UseDB: "app"}},
		{name: "use_backtick", sql: "use `bar`", want: Effect{UseDB: "bar"}},
		{name: "use_double_quote", sql: `USE "baz";`, want: Effect{UseDB: "baz"}},
		{name: "use_single_quote", sql: "USE 'qux'", want: Effect{UseDB: "qux"}},
		{name: "use_spaces", sql: "  use   db1  ", want: Effect{UseDB: "db1"}},
		{name: "use_missing", sql: "USE", want: Effect{}},
		{name: "set_names", sql: "SET NAMES utf8mb4", want: Effect{Charset: "utf8mb4"}},
		{name: "set_names_semicolon", sql: "set names utf8mb4;", want: Effect{Charset: "utf8mb4"}},
		{
			name: "set_names_quoted_collate",
			sql:  "SET NAMES `utf8mb4` COLLATE utf8mb4_bin",
			want: Effect{Charset: "utf8mb4"},
		},
		{name: "set_names_missing", sql: "SET NAMES", want: Effect{}},
		{name: "autocommit_off", sql: "SET autocommit=0", want: Effect{Autocommit: boolPtr(false)}},
		{name: "autocommit_on", sql: "SET AUTOCOMMIT = 1", want: Effect{Autocommit: boolPtr(true)}},
		{name: "autocommit_session_off", sql: "set session autocommit=off", want: Effect{Autocommit: boolPtr(false)}},
		{name: "autocommit_at_at_on", sql: "SET @@autocommit=ON", want: Effect{Autocommit: boolPtr(true)}},
		{name: "autocommit_true", sql: "SET autocommit=TRUE", want: Effect{Autocommit: boolPtr(true)}},
		{name: "autocommit_false", sql: "SET autocommit=FALSE", want: Effect{Autocommit: boolPtr(false)}},
		{name: "no_effect", sql: "SELECT 1", want: Effect{}},
		{name: "user_var", sql: "SET @x=1", want: Effect{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, classify(t, tc.sql).Effect(), tc.sql)
		})
	}
}

// TestClassify_SessionFunc migrates TestParseSessionFunc.
func TestClassify_SessionFunc(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		ok    bool
		kind  SessionFuncKind
		alias string
		col   string
	}{
		{"last_id", "SELECT LAST_INSERT_ID()", true, SessionFuncLastInsertID, "", "LAST_INSERT_ID()"},
		{"last_id_lower", "select last_insert_id()", true, SessionFuncLastInsertID, "", "last_insert_id()"},
		{"last_id_spaces", "SELECT LAST_INSERT_ID ( )", true, SessionFuncLastInsertID, "", "LAST_INSERT_ID ( )"},
		{"last_id_alias", "SELECT LAST_INSERT_ID() AS id", true, SessionFuncLastInsertID, "id", "id"},
		{"last_id_backtick_alias", "SELECT LAST_INSERT_ID() AS `last_id`", true, SessionFuncLastInsertID, "last_id", "last_id"},
		{"last_id_bare_alias", "SELECT LAST_INSERT_ID() last_id", true, SessionFuncLastInsertID, "last_id", "last_id"},
		{"last_id_semicolon", "SELECT LAST_INSERT_ID();", true, SessionFuncLastInsertID, "", "LAST_INSERT_ID()"},
		{"last_id_comment", "/* c */ SELECT LAST_INSERT_ID()", true, SessionFuncLastInsertID, "", "LAST_INSERT_ID()"},
		{"row_count", "SELECT ROW_COUNT()", true, SessionFuncRowCount, "", "ROW_COUNT()"},
		{"row_count_alias", "SELECT ROW_COUNT() AS n", true, SessionFuncRowCount, "n", "n"},
		{"connection_id", "SELECT CONNECTION_ID()", true, SessionFuncConnectionID, "", "CONNECTION_ID()"},
		{"connection_id_lower", "select connection_id()", true, SessionFuncConnectionID, "", "connection_id()"},
		{"connection_id_alias", "SELECT CONNECTION_ID() AS cid", true, SessionFuncConnectionID, "cid", "cid"},
		{"expression", "SELECT LAST_INSERT_ID() + 1", false, 0, "", ""},
		{"multi_expr", "SELECT LAST_INSERT_ID(), 1", false, 0, "", ""},
		{"found_rows", "SELECT FOUND_ROWS()", false, 0, "", ""},
		{"from", "SELECT LAST_INSERT_ID() FROM t", false, 0, "", ""},
		{"set", "SET @x = LAST_INSERT_ID()", false, 0, "", ""},
		{"missing_alias", "SELECT LAST_INSERT_ID() AS", false, 0, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(t, tc.raw).SessionFunc
			require.Equal(t, tc.ok, got != nil, tc.raw)
			if !tc.ok {
				return
			}
			require.Equal(t, tc.kind, got.Kind)
			require.Equal(t, tc.alias, got.Alias)
			require.Equal(t, tc.col, got.ColumnName())
		})
	}
}

func TestClassify_ConnectionID(t *testing.T) {
	fn := classify(t, "SELECT CONNECTION_ID()").SessionFunc
	require.NotNil(t, fn)
	require.Equal(t, "CONNECTION_ID()", fn.ColumnName())
	require.Equal(t,
		"SELECT CAST(7 AS UNSIGNED) AS `CONNECTION_ID()`",
		fn.Rewrite("7"))

	alias := classify(t, "select connection_id() as cid").SessionFunc
	require.NotNil(t, alias)
	require.Equal(t, "cid", alias.ColumnName())
}

func TestClassify_SessionVarWrite(t *testing.T) {
	cases := []struct {
		name    string
		sql     string
		session []string
		global  []string
	}{
		{name: "set_session_prefix", sql: "SET SESSION sort_buffer_size = 1", session: []string{"sort_buffer_size"}},
		{name: "set_plain", sql: "SET time_zone = '+08:00'", session: []string{"time_zone"}},
		{name: "set_at_at_session", sql: "SET @@session.time_zone='+00:00'", session: []string{"time_zone"}},
		{name: "set_at_at_plain", sql: "SET @@sort_buffer_size=1", session: []string{"sort_buffer_size"}},
		{name: "set_global", sql: "SET GLOBAL max_connections = 10", global: []string{"max_connections"}},
		{name: "set_at_at_global", sql: "SET @@GLOBAL.max_connections=10", global: []string{"max_connections"}},
		{name: "multi", sql: "SET autocommit=1, time_zone='+00:00'", session: []string{"autocommit", "time_zone"}},
		{name: "user_var_excluded", sql: "SET @x = 1", session: nil},
		{name: "set_names", sql: "SET NAMES utf8mb4", session: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := classify(t, tc.sql)
			require.Equal(t, tc.session, c.SessionVarWrite, tc.sql)
			require.Equal(t, tc.global, c.GlobalVarWrite, tc.sql)
		})
	}
}

func TestClassify_SessionVars(t *testing.T) {
	cases := []struct {
		name        string
		sql         string
		vars        map[string]string
		unsupported string
	}{
		{name: "time_zone", sql: "SET time_zone = '+08:00'", vars: map[string]string{"time_zone": "+08:00"}},
		{name: "time_zone_session", sql: "SET SESSION time_zone='SYSTEM'", vars: map[string]string{"time_zone": "SYSTEM"}},
		{
			name: "tx_isolation",
			sql:  "SET SESSION transaction_isolation = 'READ COMMITTED'",
			vars: map[string]string{"transaction_isolation": "READ COMMITTED"},
		},
		{name: "default_removes", sql: "SET time_zone = DEFAULT", vars: nil},
		{name: "autocommit_separate", sql: "SET autocommit=0", vars: nil},
		{
			name:        "non_whitelisted",
			sql:         "SET SESSION sort_buffer_size = 1",
			unsupported: "sort_buffer_size",
		},
		{name: "sql_mode_still_rejected", sql: "SET sql_mode='ANSI'", unsupported: "sql_mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := classify(t, tc.sql)
			require.Equal(t, tc.vars, c.SessionVars, tc.sql)
			if tc.unsupported != "" {
				require.Contains(t, c.UnsupportedReason(), tc.unsupported, tc.sql)
				return
			}
			require.Empty(t, c.UnsupportedReason(), tc.sql)
			if tc.vars != nil {
				require.Equal(t, tc.vars, c.Effect().Vars, tc.sql)
			}
		})
	}
}

func TestClassify_ScanErrorDegrades(t *testing.T) {
	// Unterminated input must surface as a Scan error; the proxy layer then
	// degrades conservatively (design D2).
	for _, in := range []string{"SELECT 'x", "SELECT /* x", "SELECT `x"} {
		_, err := Scan(in)
		require.Error(t, err, in)
	}
}

// TestStatement_EnsureClass covers the single-point snapshot and the D2
// conservative degradation contract.
func TestStatement_EnsureClass(t *testing.T) {
	t.Run("computed_once", func(t *testing.T) {
		stmt := &Statement{Raw: "SELECT 1"}
		first := stmt.EnsureClass()
		require.Equal(t, StmtSelect, first.Kind)
		stmt.Raw = "DELETE FROM t"
		require.Equal(t, StmtSelect, stmt.EnsureClass().Kind, "snapshot must not be recomputed")
	})
	t.Run("scan_failure_is_reserved_and_never_slave", func(t *testing.T) {
		stmt := &Statement{Raw: "SELECT 'unterminated"}
		c := stmt.EnsureClass()
		require.True(t, c.ScanFailed)
		require.True(t, c.Reserved(), "scan failure must pin the session (D2)")
		require.False(t, c.WantSlave(), "scan failure must never route to a slave (D2)")
	})
	t.Run("nil_statement", func(t *testing.T) {
		var stmt *Statement
		require.Equal(t, StmtUnknown, stmt.EnsureClass().Kind)
	})
}

// TestClassIsDML migrates TestIsDML from statement_test.go.
func TestClassIsDML(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"INSERT INTO t VALUES (1)", true},
		{"update t set a=1", true},
		{"DELETE FROM t", true},
		{"REPLACE INTO t VALUES (1)", true},
		{"LOAD DATA INFILE 'x' INTO TABLE t", true},
		{"/* c */ insert into t values (1)", true},
		{"SELECT 1", false},
		{"WITH x AS (SELECT 1) SELECT * FROM x", false},
		{"CREATE TABLE t (id INT)", false},
		{"SET autocommit=1", false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			require.Equal(t, tc.want, classify(t, tc.raw).IsDML())
		})
	}
}

// TestSessionFuncRewrite migrates TestSessionFuncRewrite from
// session_func_test.go.
func TestSessionFuncRewrite(t *testing.T) {
	cases := []struct {
		name string
		fn   SessionFunc
		val  string
		want string
	}{
		{
			name: "last_insert_id",
			fn:   SessionFunc{Kind: SessionFuncLastInsertID, Expr: "LAST_INSERT_ID()"},
			val:  "42",
			want: "SELECT CAST(42 AS UNSIGNED) AS `LAST_INSERT_ID()`",
		},
		{
			name: "row_count_alias",
			fn:   SessionFunc{Kind: SessionFuncRowCount, Alias: "n"},
			val:  "-1",
			want: "SELECT CAST(-1 AS SIGNED) AS `n`",
		},
		{
			name: "alias_escaping",
			fn:   SessionFunc{Kind: SessionFuncRowCount, Alias: "a`b"},
			val:  "0",
			want: "SELECT CAST(0 AS SIGNED) AS `a``b`",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.fn.Rewrite(tc.val))
		})
	}
}
