package session_test

import (
	"testing"
	"unsafe"

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

func TestState_ConnectionID(t *testing.T) {
	s := session.NewState()
	require.Zero(t, s.ConnectionID())

	s.SetConnectionID(7)
	require.Equal(t, uint32(7), s.ConnectionID())

	s.SetDatabase("app")
	s.SetInTx(true)
	s.Reset()
	require.Equal(t, uint32(7), s.ConnectionID(), "COM_RESET_CONNECTION keeps the same frontend session")
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

func TestState_StmtPinned(t *testing.T) {
	s := session.NewState()
	require.False(t, s.NeedsSticky())

	s.MarkStmtPinned()
	require.True(t, s.NeedsSticky())
	require.False(t, s.Reserved(), "a prepared statement pin is not a reserved SQL class")

	s.UnmarkStmtPinned()
	require.False(t, s.NeedsSticky())

	s.MarkStmtPinned()
	s.Reset()
	require.False(t, s.NeedsSticky(), "reset drops the statement pin with the stmt table")
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

// TestState_ClonesRetainedStrings pins the ownership contract: session fields
// outlive the statement, so they must not alias a caller buffer that the
// command loop may reuse (see util/zerocopy.HackString).
func TestState_ClonesRetainedStrings(t *testing.T) {
	buf := []byte("calendar")
	alias := unsafe.String(unsafe.SliceData(buf), len(buf))
	nameBuf := []byte("time_zone")
	nameAlias := unsafe.String(unsafe.SliceData(nameBuf), len(nameBuf))

	s := session.NewState()
	s.SetDatabase(alias)
	s.SetCharset(alias)
	s.SetVar(nameAlias, alias)

	buf[0] = 'X'     // caller reuses its buffer after the statement
	nameBuf[0] = 'X' // the variable name aliases the statement too

	require.Equal(t, "calendar", s.Database())
	require.Equal(t, "calendar", s.Charset())
	require.Equal(t, "calendar", s.Vars()["time_zone"], "both key and value must be owned")
}
