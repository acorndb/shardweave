package shard_test

import (
	"context"
	"testing"

	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/route/shard"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
	sqlparser "github.com/acorndb/shardweave/pkg/sql/parser"
	"github.com/stretchr/testify/require"
)

func newRouter(t *testing.T) *shard.Router {
	t.Helper()
	r, err := shard.New([]shard.Rule{{
		Table:     "orders",
		Key:       "user_id",
		Algorithm: constant.ShardingHashMod,
		Slices:    []string{"s0", "s1"},
	}})
	require.NoError(t, err)
	return r
}

func statement(t *testing.T, raw string, cmd byte) *sql.Statement {
	t.Helper()
	res, err := sqlparser.Parse(raw)
	require.NoError(t, err)
	require.Len(t, res.Stmts, 1)
	return &sql.Statement{Cmd: cmd, Raw: raw, AST: res.Stmts[0]}
}

func TestRouter_SingleShard(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		sticky   bool
		want     string
		wantRole constant.Role
	}{
		{"select_eq_s1", "SELECT * FROM orders WHERE user_id = 1", false, "s1", constant.RoleSlave},
		{"select_eq_s0", "SELECT * FROM orders WHERE user_id = 2", false, "s0", constant.RoleSlave},
		{"select_const", "SELECT 1", false, "s0", constant.RoleSlave},
		{"select_alias", "SELECT * FROM orders o WHERE o.user_id = 1", false, "s1", constant.RoleSlave},
		{"select_in_same_slice", "SELECT * FROM orders WHERE user_id IN (1, 3)", false, "s1", constant.RoleSlave},
		{"select_for_update", "SELECT * FROM orders WHERE user_id = 1 FOR UPDATE", false, "s1", constant.RoleMaster},
		{"select_in_tx", "SELECT * FROM orders WHERE user_id = 1", true, "s1", constant.RoleMaster},
		{"update", "UPDATE orders SET amount = 1 WHERE user_id = 2", false, "s0", constant.RoleMaster},
		{"delete", "DELETE FROM orders WHERE user_id = 1", false, "s1", constant.RoleMaster},
		{"insert_single_row", "INSERT INTO orders (id, user_id, amount) VALUES (1, 2, 10)", false, "s0", constant.RoleMaster},
		{"insert_same_slice_rows", "INSERT INTO orders (id, user_id, amount) VALUES (1, 2, 10), (2, 4, 20)", false, "s0", constant.RoleMaster},
		{"use", "USE bench", false, "s0", constant.RoleMaster},
		{"set_names", "SET NAMES utf8mb4", false, "s0", constant.RoleMaster},
		{"show_tables", "SHOW TABLES", false, "s0", constant.RoleSlave},
		{"show_columns", "SHOW COLUMNS FROM orders", false, "s0", constant.RoleSlave},
		{"show_create", "SHOW CREATE TABLE orders", false, "s0", constant.RoleSlave},
		{"show_index", "SHOW INDEX FROM orders", false, "s0", constant.RoleSlave},
		{"show_variables", "SHOW VARIABLES LIKE 'sql_mode'", false, "s0", constant.RoleSlave},
		{"describe", "DESC orders", false, "s0", constant.RoleSlave},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := session.NewState()
			if tc.sticky {
				st.SetInTx(true)
			}
			targets, err := newRouter(t).Route(context.Background(), st, statement(t, tc.raw, mysql.ComQuery))
			require.NoError(t, err)
			require.Len(t, targets, 1)
			require.Equal(t, tc.want, targets[0].Slice)
			require.Equal(t, tc.wantRole, targets[0].Role)
		})
	}
}

func TestRouter_Rejects(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		cmd  byte
	}{
		{"no_where", "SELECT * FROM orders", mysql.ComQuery},
		{"range", "SELECT * FROM orders WHERE user_id > 1", mysql.ComQuery},
		{"in_cross_shard", "SELECT * FROM orders WHERE user_id IN (1, 2)", mysql.ComQuery},
		{"unsharded_table", "SELECT * FROM users WHERE id = 1", mysql.ComQuery},
		{"join", "SELECT * FROM orders o JOIN users u ON u.id = o.user_id WHERE o.user_id = 1", mysql.ComQuery},
		{"schema_qualified", "SELECT * FROM app.orders WHERE user_id = 1", mysql.ComQuery},
		{"update_key", "UPDATE orders SET user_id = 3 WHERE user_id = 2", mysql.ComQuery},
		{"insert_missing_key", "INSERT INTO orders (id, amount) VALUES (1, 10)", mysql.ComQuery},
		{"insert_cross_shard", "INSERT INTO orders (id, user_id) VALUES (1, 1), (2, 2)", mysql.ComQuery},
		{"insert_select", "INSERT INTO orders (id, user_id) SELECT id, user_id FROM orders WHERE user_id = 1", mysql.ComQuery},
		{"ddl", "CREATE TABLE t (id INT)", mysql.ComQuery},
		{"show_master", "SHOW MASTER STATUS", mysql.ComQuery},
		{"show_status", "SHOW STATUS", mysql.ComQuery},
		{"explain", "EXPLAIN SELECT * FROM orders WHERE user_id = 1", mysql.ComQuery},
		{"set_other", "SET sql_mode = 'ANSI_QUOTES'", mysql.ComQuery},
		{"transaction", "BEGIN", mysql.ComQuery},
		{"prepare_insert", "INSERT INTO orders (id, user_id) VALUES (1, 2)", mysql.ComStmtPrepare},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newRouter(t).Route(context.Background(), session.NewState(), statement(t, tc.raw, tc.cmd))
			var rej *plan.RejectError
			require.ErrorAs(t, err, &rej)
			require.Equal(t, uint16(1235), rej.Code)
		})
	}
}

func TestRouter_Prepared(t *testing.T) {
	r := newRouter(t)
	st := session.NewState()

	prep := statement(t, "SELECT * FROM orders WHERE user_id = 1", mysql.ComStmtPrepare)
	targets, err := r.Route(context.Background(), st, prep)
	require.NoError(t, err)
	require.Equal(t, "s1", targets[0].Slice)
	require.Equal(t, constant.RoleMaster, targets[0].Role)

	require.NoError(t, st.Stmts().Add(&session.Stmt{
		ID: 7, Slice: "s1", Targets: map[string]uint32{"s1": 3},
	}))
	for _, cmd := range []byte{mysql.ComStmtExecute, mysql.ComStmtReset, mysql.ComStmtClose} {
		targets, err = r.Route(context.Background(), st, &sql.Statement{Cmd: cmd, StmtID: 7})
		require.NoError(t, err)
		require.Equal(t, "s1", targets[0].Slice)
		require.Equal(t, constant.RoleMaster, targets[0].Role)
	}

	_, err = r.Route(context.Background(), st, &sql.Statement{Cmd: mysql.ComStmtExecute, StmtID: 99})
	var rej *plan.RejectError
	require.ErrorAs(t, err, &rej)

	_, err = r.Route(context.Background(), st, &sql.Statement{Cmd: mysql.ComStmtPrepare})
	require.ErrorAs(t, err, &rej)
}

func TestRouter_InitDB(t *testing.T) {
	targets, err := newRouter(t).Route(context.Background(), session.NewState(), &sql.Statement{
		Cmd: mysql.ComInitDB,
		Raw: "bench",
	})
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "s0", targets[0].Slice)
	require.Equal(t, constant.RoleMaster, targets[0].Role)
}

func TestNew_RejectsInvalidRules(t *testing.T) {
	cases := []struct {
		name  string
		rules []shard.Rule
	}{
		{"empty", nil},
		{"empty_table", []shard.Rule{{Key: "id", Algorithm: constant.ShardingHashMod, Slices: []string{"s0"}}}},
		{"empty_key", []shard.Rule{{Table: "t", Algorithm: constant.ShardingHashMod, Slices: []string{"s0"}}}},
		{"bad_algorithm", []shard.Rule{{Table: "t", Key: "id", Algorithm: "range", Slices: []string{"s0"}}}},
		{"no_slices", []shard.Rule{{Table: "t", Key: "id", Algorithm: constant.ShardingHashMod}}},
		{"duplicate_table", []shard.Rule{
			{Table: "t", Key: "id", Algorithm: constant.ShardingHashMod, Slices: []string{"s0"}},
			{Table: "T", Key: "id", Algorithm: constant.ShardingHashMod, Slices: []string{"s0"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := shard.New(tc.rules)
			require.Error(t, err)
		})
	}
}
