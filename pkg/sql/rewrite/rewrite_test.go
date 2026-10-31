package rewrite_test

import (
	"strings"
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

func TestSelect_ExtraAggregates(t *testing.T) {
	sel := parseSelect(t, "SELECT AVG(amount) FROM orders")
	avg := sel.Fields.Fields[0].Expr.(*ast.AggregateFuncExpr)
	out, err := rewrite.Select(sel, rewrite.Spec{ExtraAggregates: []rewrite.ExtraAggregate{
		{Func: "sum", Expr: avg.Args[0], Alias: "__sw_avg_sum_0"},
		{Func: "count", Expr: avg.Args[0], Alias: "__sw_avg_count_0"},
	}})
	require.NoError(t, err)

	res, err := sqlparser.Parse(out)
	require.NoError(t, err)
	got := res.Stmts[0].(*ast.SelectStmt)
	require.Len(t, got.Fields.Fields, 3)
	for i, want := range []string{"avg", "sum", "count"} {
		agg, ok := got.Fields.Fields[i].Expr.(*ast.AggregateFuncExpr)
		require.True(t, ok)
		require.Equal(t, want, strings.ToLower(agg.F))
	}
	require.Equal(t, "AVG(amount)", got.Fields.Fields[0].AsName.O, "original label is preserved")
	require.Equal(t, "__sw_avg_sum_0", got.Fields.Fields[1].AsName.L)
	require.Equal(t, "__sw_avg_count_0", got.Fields.Fields[2].AsName.L)
	require.Len(t, sel.Fields.Fields, 1, "caller AST must be restored")
}

func TestSelect_ExtraFields(t *testing.T) {
	sel := parseSelect(t, "SELECT amount FROM orders ORDER BY user_id")
	order := sel.OrderBy.Items[0].Expr.(*ast.ColumnNameExpr)
	out, err := rewrite.Select(sel, rewrite.Spec{ExtraFields: []rewrite.ExtraField{{
		Expr: order, Alias: "__sw_order_0",
	}}})
	require.NoError(t, err)

	res, err := sqlparser.Parse(out)
	require.NoError(t, err)
	got := res.Stmts[0].(*ast.SelectStmt)
	require.Len(t, got.Fields.Fields, 2)
	require.Equal(t, "__sw_order_0", got.Fields.Fields[1].AsName.L)
	require.Len(t, sel.Fields.Fields, 1, "caller AST must be restored")
}

func TestSelect_RemoveLimit(t *testing.T) {
	sel := parseSelect(t, "SELECT DISTINCT amount FROM orders LIMIT 3")
	out, err := rewrite.Select(sel, rewrite.Spec{RemoveLimit: true})
	require.NoError(t, err)
	res, err := sqlparser.Parse(out)
	require.NoError(t, err)
	got := res.Stmts[0].(*ast.SelectStmt)
	require.Nil(t, got.Limit)
	require.NotNil(t, sel.Limit, "caller AST must be restored")
}

func TestSelect_RemoveHaving(t *testing.T) {
	sel := parseSelect(t, "SELECT user_id, COUNT(*) FROM orders GROUP BY user_id HAVING COUNT(*) > 1")
	out, err := rewrite.Select(sel, rewrite.Spec{RemoveHaving: true})
	require.NoError(t, err)
	res, err := sqlparser.Parse(out)
	require.NoError(t, err)
	got := res.Stmts[0].(*ast.SelectStmt)
	require.Nil(t, got.Having)
	require.NotNil(t, sel.Having, "caller AST must be restored")
}

func TestSelect_RemoveHavingKeepsLabels(t *testing.T) {
	sel := parseSelect(t, "SELECT COUNT(*) FROM orders GROUP BY user_id HAVING COUNT(*) > 1")
	out, err := rewrite.Select(sel, rewrite.Spec{RemoveHaving: true})
	require.NoError(t, err)
	res, err := sqlparser.Parse(out)
	require.NoError(t, err)
	got := res.Stmts[0].(*ast.SelectStmt)
	require.Equal(t, "COUNT(*)", got.Fields.Fields[0].AsName.O)
}

func TestSelect_ExtraGroupBy(t *testing.T) {
	sel := parseSelect(t, "SELECT COUNT(DISTINCT amount) FROM orders")
	arg := sel.Fields.Fields[0].Expr.(*ast.AggregateFuncExpr).Args[0]
	out, err := rewrite.Select(sel, rewrite.Spec{
		ExtraFields:  []rewrite.ExtraField{{Expr: arg, Alias: "__sw_distinct_0"}},
		ExtraGroupBy: []string{"__sw_distinct_0"},
	})
	require.NoError(t, err)
	res, err := sqlparser.Parse(out)
	require.NoError(t, err)
	got := res.Stmts[0].(*ast.SelectStmt)
	require.Len(t, got.Fields.Fields, 2)
	require.NotNil(t, got.GroupBy)
	require.Len(t, got.GroupBy.Items, 1)
	groupCol, ok := got.GroupBy.Items[0].Expr.(*ast.ColumnNameExpr)
	require.True(t, ok)
	require.Equal(t, "__sw_distinct_0", groupCol.Name.Name.L)
	require.Nil(t, sel.GroupBy, "caller AST must be restored")
}

func TestSelect_ExtraGroupByEmptyAlias(t *testing.T) {
	sel := parseSelect(t, "SELECT COUNT(*) FROM orders")
	_, err := rewrite.Select(sel, rewrite.Spec{ExtraGroupBy: []string{""}})
	require.Error(t, err)
	require.Len(t, sel.Fields.Fields, 1, "caller AST must be restored on error")
	require.Nil(t, sel.GroupBy, "caller AST must be restored on error")
}

func TestSelect_ExtraGroupByKeepsExistingItems(t *testing.T) {
	sel := parseSelect(t, "SELECT user_id, COUNT(DISTINCT amount) FROM orders GROUP BY user_id")
	arg := sel.Fields.Fields[1].Expr.(*ast.AggregateFuncExpr).Args[0]
	out, err := rewrite.Select(sel, rewrite.Spec{
		ExtraFields:  []rewrite.ExtraField{{Expr: arg, Alias: "__sw_distinct_0"}},
		ExtraGroupBy: []string{"__sw_distinct_0"},
	})
	require.NoError(t, err)
	res, err := sqlparser.Parse(out)
	require.NoError(t, err)
	got := res.Stmts[0].(*ast.SelectStmt)
	require.Len(t, got.GroupBy.Items, 2)
	first, ok := got.GroupBy.Items[0].Expr.(*ast.ColumnNameExpr)
	require.True(t, ok)
	require.Equal(t, "user_id", first.Name.Name.L)
	second, ok := got.GroupBy.Items[1].Expr.(*ast.ColumnNameExpr)
	require.True(t, ok)
	require.Equal(t, "__sw_distinct_0", second.Name.Name.L)
	require.Len(t, sel.GroupBy.Items, 1, "caller AST must be restored")
}
