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
	"github.com/stretchr/testify/require"
)

func stmt(raw string) *sql.Statement {
	return &sql.Statement{Cmd: mysql.ComQuery, Raw: raw}
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
