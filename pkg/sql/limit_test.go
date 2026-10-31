package sql_test

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/sql"
	sqlparser "github.com/acorndb/shardweave/pkg/sql/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/stretchr/testify/require"
)

func parseSelectLimit(t *testing.T, raw string) *ast.Limit {
	t.Helper()
	res, err := sqlparser.Parse(raw)
	require.NoError(t, err)
	require.Len(t, res.Stmts, 1)
	sel, ok := res.Stmts[0].(*ast.SelectStmt)
	require.True(t, ok)
	return sel.Limit
}

func TestParseLimit(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		offset int64
		count  int64
		ok     bool
	}{
		{"none", "SELECT 1", 0, 0, false},
		{"count", "SELECT * FROM t LIMIT 5", 0, 5, true},
		{"comma", "SELECT * FROM t LIMIT 2, 3", 2, 3, true},
		{"offset", "SELECT * FROM t LIMIT 3 OFFSET 2", 2, 3, true},
		{"zero", "SELECT * FROM t LIMIT 0", 0, 0, true},
		{"huge", "SELECT * FROM t LIMIT 18446744073709551615", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			offset, count, ok := sql.ParseLimit(parseSelectLimit(t, tc.raw))
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.offset, offset)
			require.Equal(t, tc.count, count)
		})
	}
}
