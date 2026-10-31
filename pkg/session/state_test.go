package session_test

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/session"
	"github.com/stretchr/testify/require"
)

func TestState_Defaults(t *testing.T) {
	s := session.NewState()
	require.Equal(t, "", s.Database())
	require.False(t, s.InTx())
	require.False(t, s.Reserved())
	require.True(t, s.Autocommit(), "handshake default AC=1 (kingshard/Gaea)")
	require.False(t, s.IsInTransaction())
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

// A5/A6/A7: kingshard/Gaea isInTransaction = IN_TRANS || !autocommit
func TestState_AutocommitForcesTransactionSemantics(t *testing.T) {
	s := session.NewState()
	s.SetAutocommit(false)
	require.False(t, s.InTx())
	require.True(t, s.IsInTransaction(), "AC=0 alone is in-transaction for routing")
	require.True(t, s.NeedsSticky())

	s.SetInTx(true)
	s.SetAutocommit(true) // K/G: AC=1 clears IN_TRANS
	require.False(t, s.InTx())
	require.True(t, s.Autocommit())
	require.False(t, s.IsInTransaction())
}

func TestState_Reset(t *testing.T) {
	s := session.NewState()
	s.SetDatabase("app")
	s.SetCharset("utf8mb4")
	s.SetInTx(true)
	s.MarkReserved()

	s.SetAutocommit(false)
	s.Reset()

	require.Equal(t, "app", s.Database(), "COM_RESET_CONNECTION preserves current database")
	require.Equal(t, "", s.Charset())
	require.False(t, s.InTx())
	require.False(t, s.Reserved())
	require.True(t, s.Autocommit(), "reset restores autocommit")
	require.False(t, s.NeedsSticky())
}

func TestState_SessionFunctions(t *testing.T) {
	s := session.NewState()
	require.Equal(t, uint64(0), s.LastInsertID())
	require.Equal(t, int64(-1), s.RowCount())

	s.SetLastInsertID(42)
	s.SetRowCount(3)
	require.Equal(t, uint64(42), s.LastInsertID())
	require.Equal(t, int64(3), s.RowCount())

	s.SetLastInsertID(7)
	require.Equal(t, uint64(7), s.LastInsertID(), "later inserts replace the value")

	s.Reset()
	require.Equal(t, uint64(0), s.LastInsertID(), "COM_RESET_CONNECTION clears LAST_INSERT_ID")
	require.Equal(t, int64(-1), s.RowCount(), "COM_RESET_CONNECTION resets ROW_COUNT")
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
		// @@system_var is not a user variable: reading it must not pin the session.
		{"SELECT @@server_id", false},
		{"SELECT @@GLOBAL.max_connections", false},
		{"SET @@SESSION.sql_mode = 'ANSI'", false},
		// User variable reads are connection-scoped too.
		{"SELECT @probe", true},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			require.Equal(t, tc.want, session.DetectReservedSQL(tc.sql))
		})
	}
}
