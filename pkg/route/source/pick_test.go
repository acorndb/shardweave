package source_test

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/route/source"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
	"github.com/stretchr/testify/require"
)

func query(raw string) *sql.Statement {
	return &sql.Statement{Cmd: mysql.ComQuery, Raw: raw}
}

func TestPick_CorpusA_Transaction(t *testing.T) {
	// design §5.11 A* — Layer A
	t.Run("A1", func(t *testing.T) {
		st := session.NewState()
		require.Equal(t, constant.RoleSlave, source.PickRole(st, query("SELECT 1")))
	})
	t.Run("A2", func(t *testing.T) {
		st := session.NewState()
		require.Equal(t, constant.RoleMaster, source.PickRole(st, query("BEGIN")))
	})
	t.Run("A3", func(t *testing.T) {
		st := session.NewState()
		st.SetInTx(true)
		require.Equal(t, constant.RoleMaster, source.PickRole(st, query("SELECT 1")))
	})
	t.Run("A5", func(t *testing.T) {
		// ref: kingshard isInTransaction AC=0
		st := session.NewState()
		st.SetAutocommit(false)
		require.Equal(t, constant.RoleMaster, source.PickRole(st, query("SELECT 1")))
	})
	t.Run("A9", func(t *testing.T) {
		st := session.NewState()
		st.MarkReserved()
		require.Equal(t, constant.RoleMaster, source.PickRole(st, query("SELECT 1")))
	})
	t.Run("A10", func(t *testing.T) {
		st := session.NewState()
		stmt := &sql.Statement{Cmd: mysql.ComStmtPrepare, Raw: "SELECT 1"}
		require.Equal(t, constant.RoleMaster, source.PickRole(st, stmt))
	})
}

func TestPick_COM(t *testing.T) {
	st := session.NewState()
	require.Equal(t, constant.RoleSlave,
		source.PickRole(st, &sql.Statement{Cmd: mysql.ComInitDB, Raw: "db"}))
	require.Equal(t, constant.RoleSlave,
		source.PickRole(st, &sql.Statement{Cmd: mysql.ComFieldList}))
	st.SetInTx(true)
	require.Equal(t, constant.RoleMaster,
		source.PickRole(st, &sql.Statement{Cmd: mysql.ComInitDB, Raw: "db"}))
}
