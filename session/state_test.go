package session_test

import (
	"testing"

	"github.com/sharding-proxy/session"
	"github.com/stretchr/testify/require"
)

func TestState_Defaults(t *testing.T) {
	s := session.NewState()
	require.Equal(t, "", s.Database())
	require.False(t, s.InTx())
	require.False(t, s.Reserved())
}

func TestState_SetDatabase(t *testing.T) {
	s := session.NewState()
	s.SetDatabase("app")
	require.Equal(t, "app", s.Database())
}

func TestState_TxAndReserved(t *testing.T) {
	s := session.NewState()
	s.SetInTx(true)
	require.True(t, s.InTx())
	s.MarkReserved()
	require.True(t, s.Reserved())
	s.SetInTx(false)
	require.False(t, s.InTx())
	require.True(t, s.Reserved(), "reserved sticks until disconnect")
}

func TestState_NeedsSticky(t *testing.T) {
	s := session.NewState()
	require.False(t, s.NeedsSticky())
	s.SetInTx(true)
	require.True(t, s.NeedsSticky())
	s.SetInTx(false)
	s.MarkReserved()
	require.True(t, s.NeedsSticky())
}

func TestDetectReservedSQL(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
	}{
		{"SELECT 1", false},
		{"SET @x = 1", true},
		{"set @foo := 2", true},
		{"CREATE TEMPORARY TABLE t (id INT)", true},
		{"create temporary table t(id int)", true},
		{"SELECT GET_LOCK('a', 1)", true},
		{"SELECT RELEASE_LOCK('a')", true},
		{"LOCK TABLES t READ", true},
		{"SET NAMES utf8mb4", false},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			require.Equal(t, tc.want, session.DetectReservedSQL(tc.sql))
		})
	}
}
