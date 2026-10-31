package plan_test

import (
	"context"
	"errors"
	"testing"

	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/route"
	"github.com/acorndb/shardweave/pkg/route/source"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
	sqlparser "github.com/acorndb/shardweave/pkg/sql/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/stretchr/testify/require"
)

func stmt(raw string) *sql.Statement {
	return &sql.Statement{Cmd: mysql.ComQuery, Raw: raw}
}

func selectStmt(t *testing.T, raw string) *sql.Statement {
	t.Helper()
	res, err := sqlparser.Parse(raw)
	require.NoError(t, err)
	require.Len(t, res.Stmts, 1)
	return &sql.Statement{Cmd: mysql.ComQuery, Raw: raw, AST: res.Stmts[0]}
}

func TestBuilder_AllMaster(t *testing.T) {
	p, err := (plan.Builder{Router: source.AllMaster{}}).Build(
		context.Background(), session.NewState(), stmt("SELECT 1"),
	)
	require.NoError(t, err)
	require.Equal(t, plan.Passthrough, p.Kind)
	require.Len(t, p.Targets, 1)
	require.Equal(t, route.DefaultSlice, p.Targets[0].Slice)
	require.Equal(t, constant.RoleMaster, p.Targets[0].Role)
}

func TestBuilder_ShardingSingle(t *testing.T) {
	p, err := (plan.Builder{Router: source.AllMaster{}, Sharding: true}).Build(
		context.Background(), session.NewState(), stmt("SELECT 1"))
	require.NoError(t, err)
	require.Equal(t, plan.SingleShard, p.Kind)
	require.Equal(t, plan.TxSingleShard, p.Tx)
	require.Len(t, p.Targets, 1)
}

func TestBuilder_Pick(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		sticky bool
		want   constant.Role
	}{
		{name: "select_slave", sql: "SELECT 1", want: constant.RoleSlave},
		{name: "insert_master", sql: "INSERT INTO t VALUES (1)", want: constant.RoleMaster},
		{name: "sticky_master", sql: "SELECT 1", sticky: true, want: constant.RoleMaster},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := session.NewState()
			if tc.sticky {
				st.SetInTx(true)
			}
			p, err := (plan.Builder{Router: source.Pick{}}).Build(context.Background(), st, stmt(tc.sql))
			require.NoError(t, err)
			require.Equal(t, plan.Passthrough, p.Kind)
			require.Len(t, p.Targets, 1)
			require.Equal(t, tc.want, p.Targets[0].Role)
		})
	}
}

type fakeRouter struct {
	targets []route.Target
	err     error
}

func (f fakeRouter) Route(context.Context, *session.State, *sql.Statement) ([]route.Target, error) {
	return f.targets, f.err
}

func TestBuilder_UnsupportedRejected(t *testing.T) {
	cases := []string{
		"SHOW WARNINGS",
		"SHOW MASTER STATUS",
		"SET sql_mode = 'ANSI_QUOTES'",
	}
	for _, query := range cases {
		t.Run(query, func(t *testing.T) {
			p, err := (plan.Builder{Router: source.AllMaster{}}).Build(
				context.Background(), session.NewState(), stmt(query),
			)
			require.NoError(t, err)
			require.Equal(t, plan.Reject, p.Kind)
			require.NotNil(t, p.Reject)
			require.Equal(t, uint16(1235), p.Reject.Number)
			require.Equal(t, "42000", p.Reject.SQLState)
			require.NotEmpty(t, p.Reject.Message)
		})
	}
}

func TestBuilder_MultiTargetRejected(t *testing.T) {
	p, err := (plan.Builder{Router: fakeRouter{targets: []route.Target{
		{Slice: "s0", Role: constant.RoleMaster},
		{Slice: "s1", Role: constant.RoleMaster},
	}}}).Build(context.Background(), session.NewState(), stmt("SELECT 1"))
	require.NoError(t, err)
	require.Equal(t, plan.Reject, p.Kind)
	require.NotNil(t, p.Reject)
	require.Equal(t, uint16(1105), p.Reject.Number)
	require.Contains(t, p.Reject.Error(), "multi-target")
}

func TestBuilder_MultiShard(t *testing.T) {
	targets := []route.Target{
		{Slice: "s0", Role: constant.RoleSlave},
		{Slice: "s1", Role: constant.RoleSlave},
	}
	b := plan.Builder{Router: fakeRouter{targets: targets}, Sharding: true}

	p, err := b.Build(context.Background(), session.NewState(), stmt("SELECT * FROM orders"))
	require.NoError(t, err)
	require.Equal(t, plan.MultiShard, p.Kind)
	require.Equal(t, plan.TxNone, p.Tx)
	require.Equal(t, targets, p.Targets)

	t.Run("sticky_rejected", func(t *testing.T) {
		st := session.NewState()
		st.SetInTx(true)
		p, err := b.Build(context.Background(), st, stmt("SELECT * FROM orders"))
		require.NoError(t, err)
		require.Equal(t, plan.Reject, p.Kind)
		require.Equal(t, uint16(1235), p.Reject.Number)
		require.Contains(t, p.Reject.Message, "transaction")
	})

	t.Run("autocommit_off_rejected", func(t *testing.T) {
		st := session.NewState()
		st.SetAutocommit(false)
		p, err := b.Build(context.Background(), st, stmt("SELECT * FROM orders"))
		require.NoError(t, err)
		require.Equal(t, plan.Reject, p.Kind)
		require.Equal(t, uint16(1235), p.Reject.Number)
		require.Contains(t, p.Reject.Message, "autocommit")
	})

	t.Run("limit_pushed", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(), selectStmt(t, "SELECT * FROM orders LIMIT 5"))
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.NotNil(t, p.Merge.Limit)
		require.Equal(t, int64(0), p.Merge.Limit.Offset)
		require.Equal(t, int64(5), p.Merge.Limit.Count)
		require.Empty(t, p.Targets[0].SQL, "offset 0 needs no per-shard rewrite")
	})

	t.Run("limit_zero", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(), selectStmt(t, "SELECT * FROM orders LIMIT 0"))
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.NotNil(t, p.Merge.Limit)
		require.Zero(t, p.Merge.Limit.Count)
	})

	t.Run("limit_offset_rewritten", func(t *testing.T) {
		st := selectStmt(t, "SELECT id FROM orders ORDER BY id LIMIT 2, 3")
		p, err := b.Build(context.Background(), session.NewState(), st)
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.NotNil(t, p.Merge.Limit)
		require.Equal(t, int64(2), p.Merge.Limit.Offset)
		require.Equal(t, int64(3), p.Merge.Limit.Count)
		require.Len(t, p.Targets, 2)
		for _, target := range p.Targets {
			res, err := sqlparser.Parse(target.SQL)
			require.NoError(t, err)
			sel, ok := res.Stmts[0].(*ast.SelectStmt)
			require.True(t, ok)
			offset, count, ok := sql.ParseLimit(sel.Limit)
			require.True(t, ok)
			require.Zero(t, offset)
			require.Equal(t, int64(5), count)
		}
		sel := st.AST.(*ast.SelectStmt)
		offset, count, ok := sql.ParseLimit(sel.Limit)
		require.True(t, ok)
		require.Equal(t, int64(2), offset, "plan must restore the caller's AST")
		require.Equal(t, int64(3), count)
	})

	t.Run("limit_offset_overflow_rejected", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT id FROM orders LIMIT 1 OFFSET 9223372036854775807"))
		require.NoError(t, err)
		require.Equal(t, plan.Reject, p.Kind)
		require.Equal(t, uint16(1235), p.Reject.Number)
		require.Contains(t, p.Reject.Message, "OFFSET")
	})

	t.Run("order_by_name", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT id, amount FROM orders ORDER BY id DESC"))
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.Equal(t, []plan.OrderKey{{Kind: plan.OrderByPosition, Column: 0, Desc: true}}, p.Merge.OrderBy)
	})

	t.Run("order_by_position", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT id, amount FROM orders ORDER BY 2 DESC"))
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.Equal(t, []plan.OrderKey{{Column: 1, Desc: true}}, p.Merge.OrderBy)
	})

	t.Run("order_by_hidden_column", func(t *testing.T) {
		st := selectStmt(t, "SELECT amount FROM orders ORDER BY user_id DESC")
		p, err := b.Build(context.Background(), session.NewState(), st)
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.Equal(t, []plan.OrderKey{{Kind: plan.OrderByPosition, Column: 1, Desc: true}}, p.Merge.OrderBy)
		require.Equal(t, []int{0}, p.Merge.Output)
		require.Len(t, p.Targets, 2)
		for _, target := range p.Targets {
			res, err := sqlparser.Parse(target.SQL)
			require.NoError(t, err)
			sel := res.Stmts[0].(*ast.SelectStmt)
			require.Len(t, sel.Fields.Fields, 2, "hidden ORDER BY column is appended")
		}
		orig := st.AST.(*ast.SelectStmt)
		require.Len(t, orig.Fields.Fields, 1, "caller AST must be restored")
	})

	t.Run("order_by_hidden_limit", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT amount FROM orders ORDER BY user_id LIMIT 1 OFFSET 1"))
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.Equal(t, []int{0}, p.Merge.Output)
		require.NotNil(t, p.Merge.Limit)
		require.Equal(t, int64(1), p.Merge.Limit.Offset)
		for _, target := range p.Targets {
			res, err := sqlparser.Parse(target.SQL)
			require.NoError(t, err)
			sel := res.Stmts[0].(*ast.SelectStmt)
			require.Len(t, sel.Fields.Fields, 2)
			offset, count, ok := sql.ParseLimit(sel.Limit)
			require.True(t, ok)
			require.Zero(t, offset)
			require.Equal(t, int64(2), count)
		}
	})

	t.Run("order_by_projected_alias", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT amount AS a FROM orders ORDER BY a"))
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.Empty(t, p.Merge.Output)
		require.Equal(t, []plan.OrderKey{{Kind: plan.OrderByPosition, Column: 0}}, p.Merge.OrderBy)
	})

	t.Run("order_by_wildcard", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT * FROM orders ORDER BY user_id"))
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.Empty(t, p.Merge.Output)
		require.Equal(t, []plan.OrderKey{{Kind: plan.OrderByName, Name: "user_id"}}, p.Merge.OrderBy)
	})

	t.Run("order_by_limit", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT id FROM orders ORDER BY id LIMIT 5"))
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.Len(t, p.Merge.OrderBy, 1)
		require.NotNil(t, p.Merge.Limit)
		require.Equal(t, int64(5), p.Merge.Limit.Count)
	})

	t.Run("order_by_expression", func(t *testing.T) {
		st := selectStmt(t, "SELECT amount FROM orders ORDER BY amount + 1 DESC")
		p, err := b.Build(context.Background(), session.NewState(), st)
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.Equal(t, []plan.OrderKey{{Kind: plan.OrderByPosition, Column: 1, Desc: true}}, p.Merge.OrderBy)
		require.Equal(t, []int{0}, p.Merge.Output)
		for _, target := range p.Targets {
			res, err := sqlparser.Parse(target.SQL)
			require.NoError(t, err)
			sel := res.Stmts[0].(*ast.SelectStmt)
			require.Len(t, sel.Fields.Fields, 2)
		}
		require.Len(t, st.AST.(*ast.SelectStmt).Fields.Fields, 1)
	})

	t.Run("order_by_expression_wildcard_rejected", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT * FROM orders ORDER BY amount + 1"))
		require.NoError(t, err)
		require.Equal(t, plan.Reject, p.Kind)
		require.Equal(t, uint16(1235), p.Reject.Number)
		require.Contains(t, p.Reject.Message, "explicit select")
	})

	t.Run("aggregate_count", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT COUNT(*) FROM orders"))
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.Equal(t, []plan.AggregateSpec{{Func: plan.AggCount, Column: 0, Output: 0}}, p.Merge.Aggregates)
	})

	t.Run("aggregate_multi", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT COUNT(*), SUM(amount), MIN(amount), MAX(amount) FROM orders"))
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.Len(t, p.Merge.Aggregates, 4)
		require.Equal(t, plan.AggCount, p.Merge.Aggregates[0].Func)
		require.Equal(t, plan.AggSum, p.Merge.Aggregates[1].Func)
		require.Equal(t, plan.AggMin, p.Merge.Aggregates[2].Func)
		require.Equal(t, plan.AggMax, p.Merge.Aggregates[3].Func)
	})

	t.Run("aggregate_distinct_rejected", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT COUNT(DISTINCT user_id) FROM orders"))
		require.NoError(t, err)
		require.Equal(t, plan.Reject, p.Kind)
		require.Equal(t, uint16(1235), p.Reject.Number)
		require.Contains(t, p.Reject.Message, "DISTINCT")
	})

	t.Run("aggregate_avg", func(t *testing.T) {
		st := selectStmt(t, "SELECT AVG(amount) FROM orders")
		p, err := b.Build(context.Background(), session.NewState(), st)
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.Equal(t, []plan.AggregateSpec{{
			Func: plan.AggAvg, Column: 1, CountColumn: 2, Output: 0,
		}}, p.Merge.Aggregates)
		require.Len(t, p.Targets, 2)
		for _, target := range p.Targets {
			res, err := sqlparser.Parse(target.SQL)
			require.NoError(t, err)
			sel := res.Stmts[0].(*ast.SelectStmt)
			require.Len(t, sel.Fields.Fields, 3, "AVG needs hidden SUM/COUNT")
		}
		orig := st.AST.(*ast.SelectStmt)
		require.Len(t, orig.Fields.Fields, 1, "caller AST must be restored")
	})

	t.Run("aggregate_avg_multi", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT COUNT(*), AVG(amount) FROM orders"))
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.Equal(t, []plan.AggregateSpec{
			{Func: plan.AggCount, Column: 0, Output: 0},
			{Func: plan.AggAvg, Column: 2, CountColumn: 3, Output: 1},
		}, p.Merge.Aggregates)
	})

	t.Run("distinct", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT DISTINCT user_id, amount FROM orders"))
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.True(t, p.Merge.Distinct)
		require.Empty(t, p.Merge.Aggregates)
	})

	t.Run("distinct_order", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT DISTINCT user_id FROM orders ORDER BY user_id"))
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.True(t, p.Merge.Distinct)
		require.Equal(t, []plan.OrderKey{{Kind: plan.OrderByPosition, Column: 0}}, p.Merge.OrderBy)
	})

	t.Run("distinct_aggregate_rejected", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT DISTINCT COUNT(*) FROM orders"))
		require.NoError(t, err)
		require.Equal(t, plan.Reject, p.Kind)
		require.Contains(t, p.Reject.Message, "DISTINCT")
	})

	t.Run("group_by", func(t *testing.T) {
		st := selectStmt(t, "SELECT user_id, COUNT(*), SUM(amount), AVG(amount) FROM orders GROUP BY user_id")
		p, err := b.Build(context.Background(), session.NewState(), st)
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.Equal(t, []int{0}, p.Merge.GroupBy)
		require.Equal(t, []int{0, 1, 2, 3}, p.Merge.Output)
		require.Equal(t, []plan.AggregateSpec{
			{Func: plan.AggCount, Column: 1, Output: 1},
			{Func: plan.AggSum, Column: 2, Output: 2},
			{Func: plan.AggAvg, Column: 4, CountColumn: 5, Output: 3},
		}, p.Merge.Aggregates)
		for _, target := range p.Targets {
			res, err := sqlparser.Parse(target.SQL)
			require.NoError(t, err)
			sel := res.Stmts[0].(*ast.SelectStmt)
			require.Len(t, sel.Fields.Fields, 6, "AVG hidden SUM/COUNT appended")
		}
		require.Len(t, st.AST.(*ast.SelectStmt).Fields.Fields, 4)
	})

	t.Run("group_by_position_no_aggregate", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT user_id FROM orders GROUP BY 1"))
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.Equal(t, []int{0}, p.Merge.GroupBy)
		require.Empty(t, p.Merge.Aggregates)
		require.Equal(t, []int{0}, p.Merge.Output)
	})

	t.Run("group_by_unselected_rejected", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT COUNT(*) FROM orders GROUP BY user_id"))
		require.NoError(t, err)
		require.Equal(t, plan.Reject, p.Kind)
		require.Contains(t, p.Reject.Message, "GROUP BY")
	})

	t.Run("group_by_expression_rejected", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT amount, COUNT(*) FROM orders GROUP BY amount + 1"))
		require.NoError(t, err)
		require.Equal(t, plan.Reject, p.Kind)
		require.Contains(t, p.Reject.Message, "GROUP BY")
	})

	t.Run("group_by_mixed_rejected", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT user_id, amount, COUNT(*) FROM orders GROUP BY user_id"))
		require.NoError(t, err)
		require.Equal(t, plan.Reject, p.Kind)
		require.Contains(t, p.Reject.Message, "aggregate")
	})

	t.Run("group_by_order_limit", func(t *testing.T) {
		st := selectStmt(t, "SELECT amount, COUNT(*) FROM orders GROUP BY amount ORDER BY amount DESC LIMIT 1")
		p, err := b.Build(context.Background(), session.NewState(), st)
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.Equal(t, []int{0}, p.Merge.GroupBy)
		require.Equal(t, []plan.OrderKey{{Kind: plan.OrderByPosition, Column: 0, Desc: true}}, p.Merge.OrderBy)
		require.NotNil(t, p.Merge.Limit)
		require.Equal(t, int64(1), p.Merge.Limit.Count)
		for _, target := range p.Targets {
			res, err := sqlparser.Parse(target.SQL)
			require.NoError(t, err)
			sel := res.Stmts[0].(*ast.SelectStmt)
			require.Nil(t, sel.Limit, "GROUP BY LIMIT is applied after the global merge")
		}
	})

	t.Run("distinct_order_limit", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT DISTINCT amount FROM orders ORDER BY amount DESC LIMIT 1"))
		require.NoError(t, err)
		require.Equal(t, plan.MultiShard, p.Kind)
		require.True(t, p.Merge.Distinct)
		require.Equal(t, []plan.OrderKey{{Kind: plan.OrderByPosition, Column: 0, Desc: true}}, p.Merge.OrderBy)
		require.NotNil(t, p.Merge.Limit)
		for _, target := range p.Targets {
			res, err := sqlparser.Parse(target.SQL)
			require.NoError(t, err)
			require.Nil(t, res.Stmts[0].(*ast.SelectStmt).Limit)
		}
	})

	t.Run("distinct_order_unselected_rejected", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT DISTINCT amount FROM orders ORDER BY user_id"))
		require.NoError(t, err)
		require.Equal(t, plan.Reject, p.Kind)
		require.Contains(t, p.Reject.Message, "ORDER BY")
	})

	t.Run("aggregate_avg_distinct_rejected", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT AVG(DISTINCT amount) FROM orders"))
		require.NoError(t, err)
		require.Equal(t, plan.Reject, p.Kind)
		require.Equal(t, uint16(1235), p.Reject.Number)
		require.Contains(t, p.Reject.Message, "DISTINCT")
	})

	t.Run("aggregate_mixed_rejected", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT COUNT(*), amount FROM orders"))
		require.NoError(t, err)
		require.Equal(t, plan.Reject, p.Kind)
		require.Equal(t, uint16(1235), p.Reject.Number)
		require.Contains(t, p.Reject.Message, "aggregate")
	})

	t.Run("aggregate_order_rejected", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT COUNT(*) FROM orders ORDER BY 1"))
		require.NoError(t, err)
		require.Equal(t, plan.Reject, p.Kind)
		require.Equal(t, uint16(1235), p.Reject.Number)
		require.Contains(t, p.Reject.Message, "ORDER BY")
	})

	t.Run("aggregate_expression_rejected", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT COUNT(*) + 1 FROM orders"))
		require.NoError(t, err)
		require.Equal(t, plan.Reject, p.Kind)
		require.Equal(t, uint16(1235), p.Reject.Number)
		require.Contains(t, p.Reject.Message, "aggregate")
	})

	t.Run("aggregate_subquery_rejected", func(t *testing.T) {
		p, err := b.Build(context.Background(), session.NewState(),
			selectStmt(t, "SELECT (SELECT COUNT(*) FROM users) FROM orders"))
		require.NoError(t, err)
		require.Equal(t, plan.Reject, p.Kind)
		require.Equal(t, uint16(1235), p.Reject.Number)
		require.Contains(t, p.Reject.Message, "aggregate")
	})
}

func TestBuilder_Errors(t *testing.T) {
	t.Run("nil_router", func(t *testing.T) {
		_, err := (plan.Builder{}).Build(context.Background(), session.NewState(), stmt("SELECT 1"))
		require.Error(t, err)
	})
	t.Run("nil_statement", func(t *testing.T) {
		_, err := (plan.Builder{Router: source.AllMaster{}}).Build(context.Background(), session.NewState(), nil)
		require.Error(t, err)
	})
	t.Run("router_error", func(t *testing.T) {
		_, err := (plan.Builder{Router: fakeRouter{err: errors.New("route failed")}}).
			Build(context.Background(), session.NewState(), stmt("SELECT 1"))
		require.Error(t, err)
	})
	t.Run("no_targets", func(t *testing.T) {
		_, err := (plan.Builder{Router: fakeRouter{}}).
			Build(context.Background(), session.NewState(), stmt("SELECT 1"))
		require.Error(t, err)
	})
}

func TestKind_String(t *testing.T) {
	cases := map[plan.Kind]string{
		plan.Passthrough: "Passthrough",
		plan.SingleShard: "SingleShard",
		plan.MultiShard:  "MultiShard",
		plan.Reject:      "Reject",
		plan.Kind(99):    "Unknown",
	}
	for k, want := range cases {
		require.Equal(t, want, k.String())
	}
}

func TestPlan_ShardingContracts(t *testing.T) {
	p := plan.Plan{
		Kind: plan.SingleShard,
		Tx:   plan.TxSingleShard,
		Merge: plan.MergeSpec{
			OrderBy:    []plan.OrderKey{{Column: 0, Desc: true}},
			Limit:      &plan.Limit{Offset: 1, Count: 2},
			Aggregates: []plan.AggregateSpec{{Func: plan.AggSum, Column: 1, Output: 0}},
			GroupBy:    []int{0},
			Distinct:   true,
			Output:     []int{0},
		},
	}
	require.Equal(t, plan.SingleShard, p.Kind)
	require.Equal(t, plan.TxSingleShard, p.Tx)
	require.Equal(t, "TxSingleShard", p.Tx.String())
	require.Equal(t, int64(1), p.Merge.Limit.Offset)
	require.Equal(t, plan.AggSum, p.Merge.Aggregates[0].Func)
	require.Equal(t, "TxNone", plan.TxNone.String())
	require.Equal(t, "TxReject", plan.TxReject.String())
}
