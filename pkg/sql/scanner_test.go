package sql

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// renderTokens renders a token list as "kind(text) ..." for table-driven
// comparisons. TokenKind.String is defined in scanner.go.
func renderTokens(ts []Token) string {
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = fmt.Sprintf("%s(%s)", t.Kind, t.Text)
	}
	return strings.Join(parts, " ")
}

func TestScan_Table(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "select_basic",
			in:   "SELECT id FROM t WHERE x = 1",
			want: "ident(SELECT) ident(id) ident(FROM) ident(t) ident(WHERE) ident(x) op(=) num(1)",
		},
		{name: "lowercase", in: "select 1", want: "ident(select) num(1)"},
		{name: "string_doubled_quote", in: `SELECT 'a''b'`, want: "ident(SELECT) str(a''b)"},
		{name: "string_backslash_escape", in: `SELECT "c\"d"`, want: `ident(SELECT) str(c\"d)`},
		{
			name: "quoted_ident",
			in:   "SELECT `weird name` FROM `t``x`",
			want: "ident(SELECT) qident(weird name) ident(FROM) qident(t``x)",
		},
		{name: "line_comment", in: "-- c\nSELECT 1", want: "comment( c) ident(SELECT) num(1)"},
		{name: "hash_comment", in: "# c\nSELECT 1", want: "comment( c) ident(SELECT) num(1)"},
		{name: "block_comment", in: "/*master*/SELECT 1", want: "comment(master) ident(SELECT) num(1)"},
		{name: "exec_comment", in: "/*!40101 SET @x=1 */", want: "exec(40101 SET @x=1 )"},
		{name: "user_var_assign", in: "@x := 1", want: "uvar(x) op(:=) num(1)"},
		{name: "system_var_scope", in: "@@session.autocommit", want: "svar(session.autocommit)"},
		{name: "system_var_plain", in: "@@server_id", want: "svar(server_id)"},
		{name: "qualified_column", in: "t.id", want: "ident(t) punct(.) ident(id)"},
		{name: "numbers", in: "1.5 .5 0x1F 1e10", want: "num(1.5) num(.5) num(0x1F) num(1e10)"},
		{
			name: "operators",
			in:   "a<=b AND c>=d AND e<>f AND g!=h AND i<=>j",
			want: "ident(a) op(<=) ident(b) ident(AND) ident(c) op(>=) ident(d) " +
				"ident(AND) ident(e) op(<>) ident(f) ident(AND) ident(g) op(!=) " +
				"ident(h) ident(AND) ident(i) op(<=>) ident(j)",
		},
		{name: "punct", in: "(a,b);", want: "punct(() ident(a) punct(,) ident(b) punct()) punct(;)"},
		{
			name: "prepared_placeholder",
			in:   "SELECT * FROM t WHERE id = ?",
			want: "ident(SELECT) op(*) ident(FROM) ident(t) ident(WHERE) ident(id) op(=) punct(?)",
		},
		{
			name: "set_names_quoted",
			in:   "SET NAMES `utf8mb4`",
			want: "ident(SET) ident(NAMES) qident(utf8mb4)",
		},
		{name: "empty", in: "   ", want: ""},
		{name: "only_comment", in: "-- only", want: "comment( only)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toks, err := Scan(tc.in)
			require.NoError(t, err, tc.in)
			require.Equal(t, tc.want, renderTokens(toks), tc.in)
		})
	}
}

func TestScan_KeywordUpper(t *testing.T) {
	toks, err := Scan("select Foo")
	require.NoError(t, err)
	require.Len(t, toks, 2)
	require.Equal(t, "SELECT", toks[0].Upper)
	require.Equal(t, "select", toks[0].Text)
	require.Equal(t, "FOO", toks[1].Upper)
}

func TestScan_Positions(t *testing.T) {
	toks, err := Scan("SELECT 1, 'x'")
	require.NoError(t, err)
	require.Len(t, toks, 4)
	require.Equal(t, 0, toks[0].Pos)
	require.Equal(t, 7, toks[1].Pos)
	require.Equal(t, 8, toks[2].Pos)
	require.Equal(t, 10, toks[3].Pos)
	for i := 1; i < len(toks); i++ {
		require.Greater(t, toks[i].Pos, toks[i-1].Pos, "positions must increase")
	}
}

func TestScan_Errors(t *testing.T) {
	cases := []struct{ name, in string }{
		{name: "unterminated_string", in: "SELECT 'abc"},
		{name: "unterminated_quoted_ident", in: "SELECT `abc"},
		{name: "unterminated_block_comment", in: "SELECT /*abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Scan(tc.in)
			require.Error(t, err, tc.in)
		})
	}
}

// FuzzScan checks that the scanner never panics and that token positions are
// strictly increasing (each token consumes at least one byte).
func FuzzScan(f *testing.F) {
	seeds := []string{
		"SELECT 1",
		"SELECT 'a''b', `c``d` FROM t WHERE x<=1",
		"/*master*/ SELECT @@session.autocommit",
		"-- c\nSET @x := 1",
		"/*!40101 SET NAMES utf8mb4 */",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		toks, err := Scan(in)
		if err != nil {
			return
		}
		for i := 1; i < len(toks); i++ {
			require.Greater(t, toks[i].Pos, toks[i-1].Pos, "pos must increase")
		}
	})
}
