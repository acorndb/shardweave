package source_test

import (
	"testing"

	"github.com/acorndb/prism/pkg/constant"
	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/route/source"
	"github.com/acorndb/prism/pkg/session"
	"github.com/stretchr/testify/require"
)

func TestPick_CorpusA_Transaction(t *testing.T) {
	// design §5.11 A* — Layer A
	t.Run("A1", func(t *testing.T) {
		st := session.NewState()
		require.Equal(t, constant.RoleSlave, source.Pick(st, mysql.ComQuery, "SELECT 1"))
	})
	t.Run("A2", func(t *testing.T) {
		st := session.NewState()
		require.Equal(t, constant.RoleMaster, source.Pick(st, mysql.ComQuery, "BEGIN"))
	})
	t.Run("A3", func(t *testing.T) {
		st := session.NewState()
		st.SetInTx(true)
		require.Equal(t, constant.RoleMaster, source.Pick(st, mysql.ComQuery, "SELECT 1"))
	})
	t.Run("A5", func(t *testing.T) {
		// ref: kingshard isInTransaction AC=0
		st := session.NewState()
		st.SetAutocommit(false)
		require.Equal(t, constant.RoleMaster, source.Pick(st, mysql.ComQuery, "SELECT 1"))
	})
	t.Run("A9", func(t *testing.T) {
		st := session.NewState()
		st.MarkReserved()
		require.Equal(t, constant.RoleMaster, source.Pick(st, mysql.ComQuery, "SELECT 1"))
	})
	t.Run("A10", func(t *testing.T) {
		st := session.NewState()
		require.Equal(t, constant.RoleMaster, source.Pick(st, mysql.ComStmtPrepare, "SELECT 1"))
	})
}

func TestPick_COM(t *testing.T) {
	st := session.NewState()
	require.Equal(t, constant.RoleSlave, source.Pick(st, mysql.ComInitDB, "db"))
	require.Equal(t, constant.RoleSlave, source.Pick(st, mysql.ComFieldList, ""))
	st.SetInTx(true)
	require.Equal(t, constant.RoleMaster, source.Pick(st, mysql.ComInitDB, "db"))
}
