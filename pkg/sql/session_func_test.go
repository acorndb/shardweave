package sql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSessionFunc(t *testing.T) {
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
		{"expression", "SELECT LAST_INSERT_ID() + 1", false, 0, "", ""},
		{"multi_expr", "SELECT LAST_INSERT_ID(), 1", false, 0, "", ""},
		{"found_rows", "SELECT FOUND_ROWS()", false, 0, "", ""},
		{"from", "SELECT LAST_INSERT_ID() FROM t", false, 0, "", ""},
		{"set", "SET @x = LAST_INSERT_ID()", false, 0, "", ""},
		{"missing_alias", "SELECT LAST_INSERT_ID() AS", false, 0, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn, ok := ParseSessionFunc(tc.raw)
			require.Equal(t, tc.ok, ok, tc.raw)
			if !tc.ok {
				return
			}
			require.Equal(t, tc.kind, fn.Kind)
			require.Equal(t, tc.alias, fn.Alias)
			require.Equal(t, tc.col, fn.ColumnName())
		})
	}
}

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
