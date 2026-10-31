package plan_test

import (
	"context"
	"errors"
	"testing"

	"github.com/acorndb/prism/pkg/constant"
	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/plan"
	"github.com/acorndb/prism/pkg/route"
	"github.com/acorndb/prism/pkg/route/source"
	"github.com/acorndb/prism/pkg/session"
	"github.com/acorndb/prism/pkg/sql"
	"github.com/stretchr/testify/require"
)

func stmt(raw string) *sql.Statement {
	return &sql.Statement{Cmd: mysql.ComQuery, Raw: raw}
}

func TestBuilder_AllMaster(t *testing.T) {
	p, err := (plan.Builder{Router: source.AllMaster{}}).Build(
		context.Background(), session.NewState(), stmt("SELECT 1"))
	require.NoError(t, err)
	require.Equal(t, plan.Passthrough, p.Kind)
	require.Len(t, p.Targets, 1)
	require.Equal(t, route.DefaultSlice, p.Targets[0].Slice)
	require.Equal(t, constant.RoleMaster, p.Targets[0].Role)
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

func TestBuilder_MultiTargetRejected(t *testing.T) {
	p, err := (plan.Builder{Router: fakeRouter{targets: []route.Target{
		{Slice: "s0", Role: constant.RoleMaster},
		{Slice: "s1", Role: constant.RoleMaster},
	}}}).Build(context.Background(), session.NewState(), stmt("SELECT 1"))
	require.NoError(t, err)
	require.Equal(t, plan.Reject, p.Kind)
	require.NotNil(t, p.Reject)
	require.Equal(t, uint16(1105), p.Reject.Code)
	require.Contains(t, p.Reject.Error(), "multi-target")
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
