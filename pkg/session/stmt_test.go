package session_test

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/session"
	"github.com/stretchr/testify/require"
)

func TestStmtTable(t *testing.T) {
	tbl := session.NewStmtTable()
	require.Zero(t, tbl.Len())

	st := &session.Stmt{ID: 1, SQL: "SELECT ?", Params: 1, Targets: map[string]uint32{"s0": 7}}
	require.NoError(t, tbl.Add(st))
	require.Equal(t, 1, tbl.Len())

	got, ok := tbl.Get(1)
	require.True(t, ok)
	require.Equal(t, st, got)

	require.Error(t, tbl.Add(st), "duplicate id must fail")
	require.Error(t, tbl.Add(nil))

	require.True(t, tbl.Delete(1))
	require.False(t, tbl.Delete(1))
	require.Zero(t, tbl.Len())
}

func TestState_StmtTable(t *testing.T) {
	st := session.NewState()
	require.NotNil(t, st.Stmts())
	require.NoError(t, st.Stmts().Add(&session.Stmt{ID: 1, SQL: "SELECT ?"}))

	st.Reset()
	require.Zero(t, st.Stmts().Len(), "COM_RESET_CONNECTION clears prepared statements")
}
