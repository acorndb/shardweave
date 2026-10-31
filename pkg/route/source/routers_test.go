package source_test

import (
	"context"
	"testing"

	"github.com/acorndb/prism/pkg/constant"
	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/route"
	"github.com/acorndb/prism/pkg/route/source"
	"github.com/acorndb/prism/pkg/session"
	"github.com/acorndb/prism/pkg/sql"
	"github.com/stretchr/testify/require"
)

func TestAllMaster_Route(t *testing.T) {
	targets, err := (source.AllMaster{}).Route(context.Background(), session.NewState(), &sql.Statement{})
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, route.DefaultSlice, targets[0].Slice)
	require.Equal(t, constant.RoleMaster, targets[0].Role)

	targets, err = (source.AllMaster{Slice: "primary"}).Route(context.Background(), session.NewState(), &sql.Statement{})
	require.NoError(t, err)
	require.Equal(t, "primary", targets[0].Slice)
}

func TestPick_Route(t *testing.T) {
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
			stmt := &sql.Statement{Cmd: mysql.ComQuery, Raw: tc.sql}
			targets, err := (source.Pick{}).Route(context.Background(), st, stmt)
			require.NoError(t, err)
			require.Len(t, targets, 1)
			require.Equal(t, route.DefaultSlice, targets[0].Slice)
			require.Equal(t, tc.want, targets[0].Role)
		})
	}
}

func TestPick_RouteNilStatement(t *testing.T) {
	_, err := (source.Pick{}).Route(context.Background(), session.NewState(), nil)
	require.Error(t, err)
}

func TestPick_RouteExplicitSlice(t *testing.T) {
	targets, err := (source.Pick{Slice: "primary"}).Route(
		context.Background(), session.NewState(), &sql.Statement{Cmd: mysql.ComQuery, Raw: "SELECT 1"})
	require.NoError(t, err)
	require.Equal(t, "primary", targets[0].Slice)
}
