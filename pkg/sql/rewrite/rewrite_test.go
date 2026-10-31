package rewrite_test

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/sql"
	sqlparser "github.com/acorndb/shardweave/pkg/sql/parser"
	"github.com/acorndb/shardweave/pkg/sql/rewrite"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/stretchr/testify/require"
)

func parseSelect(t *testing.T, raw string) *ast.SelectStmt {
	t.Helper()
	res, err := sqlparser.Parse(raw)
	require.NoError(t, err)
	require.Len(t, res.Stmts, 1)
	sel, ok := res.Stmts[0].(*ast.SelectStmt)
	require.True(t, ok)
	return sel
}

func TestSelect_PerShardLimit(t *testing.T) {
	sel := parseSelect(t, "SELECT id FROM orders ORDER BY id LIMIT 2, 3")
	limit := int64(5)
	out, err := rewrite.Select(sel, rewrite.Spec{PerShardLimit: &limit})
	require.NoError(t, err)
	require.Contains(t, out, "LIMIT 5")

	res, err := sqlparser.Parse(out)
	require.NoError(t, err)
	got := res.Stmts[0].(*ast.SelectStmt)
	offset, count, ok := sql.ParseLimit(got.Limit)
	require.True(t, ok)
	require.Zero(t, offset)
	require.Equal(t, int64(5), count)

	offset, count, ok = sql.ParseLimit(sel.Limit)
	require.True(t, ok)
	require.Equal(t, int64(2), offset, "caller AST must be restored")
	require.Equal(t, int64(3), count)
}

func TestSelect_NoSpec(t *testing.T) {
	sel := parseSelect(t, "SELECT id FROM orders LIMIT 2")
	out, err := rewrite.Select(sel, rewrite.Spec{})
	require.NoError(t, err)
	require.Contains(t, out, "LIMIT 2")

	_, err = rewrite.Select(nil, rewrite.Spec{})
	require.Error(t, err)
}
