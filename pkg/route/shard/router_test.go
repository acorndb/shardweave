package shard_test

import (
	"context"
	"testing"

	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/route/shard"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
	sqlparser "github.com/acorndb/shardweave/pkg/sql/parser"
	"github.com/stretchr/testify/require"
)

func newRouter(t *testing.T) *shard.Router {
	t.Helper()
	return routerWith(t, constant.ShardingHashMod)
}

func routerWith(t *testing.T, algorithm string) *shard.Router {
	t.Helper()
	r, err := shard.New([]shard.Rule{{
		Table:     "orders",
		Key:       "user_id",
		Algorithm: algorithm,
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

func TestRouter_MultiShardSelect(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"scatter_all", "SELECT id, amount FROM orders", []string{"s0", "s1"}},
		{"scatter_range", "SELECT * FROM orders WHERE user_id > 1", []string{"s0", "s1"}},
		{"scatter_partial_or", "SELECT * FROM orders WHERE user_id = 1 OR amount > 10", []string{"s0", "s1"}},
		{"in_cross_shard", "SELECT * FROM orders WHERE user_id IN (1, 2)", []string{"s0", "s1"}},
		{"in_rule_order", "SELECT * FROM orders WHERE user_id IN (2, 1)", []string{"s0", "s1"}},
		{"in_same_slice", "SELECT * FROM orders WHERE user_id IN (1, 3)", []string{"s1"}},
		{"keyed_single", "SELECT * FROM orders WHERE user_id = 2", []string{"s0"}},
		{"keyed_single_order_by", "SELECT * FROM orders WHERE user_id = 2 ORDER BY id", []string{"s0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			targets, err := newRouter(t).Route(context.Background(), session.NewState(), statement(t, tc.raw, mysql.ComQuery))
			require.NoError(t, err)
			slices := make([]string, 0, len(targets))
			for _, target := range targets {
				require.Equal(t, constant.RoleSlave, target.Role)
				require.Empty(t, target.SQL, "scatter sends the original SQL")
				slices = append(slices, target.Slice)
			}
			require.Equal(t, tc.want, slices)
		})
	}
}

func TestRouter_KeyTypes(t *testing.T) {
	t.Run("hash_mod_integer", func(t *testing.T) {
		cases := []struct {
			name string
			raw  string
			want string
		}{
			{"int_literal", "SELECT * FROM orders WHERE user_id = 1", "s1"},
			{"numeric_string", "SELECT * FROM orders WHERE user_id = '1'", "s1"},
			{"leading_zero", "SELECT * FROM orders WHERE user_id = '01'", "s1"},
			{"plus_sign", "SELECT * FROM orders WHERE user_id = '+1'", "s1"},
			{"negative", "SELECT * FROM orders WHERE user_id = -1", "s1"},
			{"negative_string", "SELECT * FROM orders WHERE user_id = '-1'", "s1"},
			{"in_mixed_literal_forms", "SELECT * FROM orders WHERE user_id IN (1, '3')", "s1"},
			{"insert_numeric_string", "INSERT INTO orders (id, user_id) VALUES (1, '1')", "s1"},
		}
		r := routerWith(t, constant.ShardingHashMod)
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				targets, err := r.Route(context.Background(), session.NewState(), statement(t, tc.raw, mysql.ComQuery))
				require.NoError(t, err)
				require.Len(t, targets, 1)
				require.Equal(t, tc.want, targets[0].Slice)
			})
		}
	})

	t.Run("hash_mod_rejects_non_integer", func(t *testing.T) {
		r := routerWith(t, constant.ShardingHashMod)
		for _, raw := range []string{
			"SELECT * FROM orders WHERE user_id = 'abc'",
			"SELECT * FROM orders WHERE user_id = '1.0'",
			"SELECT * FROM orders WHERE user_id = 1.0",
			"SELECT * FROM orders WHERE user_id = 0x7b",
			"SELECT * FROM orders WHERE user_id IN (1, 'x')",
			"INSERT INTO orders (id, user_id) VALUES (1, 'x')",
			"UPDATE orders SET amount = 1 WHERE user_id = 'x'",
		} {
			t.Run(raw, func(t *testing.T) {
				_, err := r.Route(context.Background(), session.NewState(), statement(t, raw, mysql.ComQuery))
				var rej *errs.MySQL
				require.ErrorAs(t, err, &rej)
				require.Equal(t, uint16(1235), rej.Number)
			})
		}
	})

	t.Run("hash_mod_str_strings", func(t *testing.T) {
		r := routerWith(t, constant.ShardingHashModStr)
		cases := []struct {
			name string
			raw  string
			want []string
		}{
			{"alice", "SELECT * FROM orders WHERE user_id = 'alice'", []string{"s1"}},
			{"bob", "SELECT * FROM orders WHERE user_id = 'bob'", []string{"s0"}},
			{"in", "SELECT * FROM orders WHERE user_id IN ('alice', 'bob')", []string{"s0", "s1"}},
			{"insert", "INSERT INTO orders (id, user_id) VALUES (1, 'alice')", []string{"s1"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				targets, err := r.Route(context.Background(), session.NewState(), statement(t, tc.raw, mysql.ComQuery))
				require.NoError(t, err)
				slices := make([]string, 0, len(targets))
				for _, target := range targets {
					slices = append(slices, target.Slice)
				}
				require.Equal(t, tc.want, slices)
			})
		}
	})

	t.Run("hash_mod_str_rejects_non_string", func(t *testing.T) {
		r := routerWith(t, constant.ShardingHashModStr)
		for _, raw := range []string{
			"SELECT * FROM orders WHERE user_id = 1",
			"SELECT * FROM orders WHERE user_id = 1.0",
			"SELECT * FROM orders WHERE user_id = 0x7b",
			"SELECT * FROM orders WHERE user_id IN ('a', 1)",
			"INSERT INTO orders (id, user_id) VALUES (1, 1)",
			"UPDATE orders SET amount = 1 WHERE user_id = 1",
		} {
			t.Run(raw, func(t *testing.T) {
				_, err := r.Route(context.Background(), session.NewState(), statement(t, raw, mysql.ComQuery))
				var rej *errs.MySQL
				require.ErrorAs(t, err, &rej)
				require.Equal(t, uint16(1235), rej.Number)
			})
		}
	})
}

func TestRouter_Rejects(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		cmd  byte
	}{
		{"unsharded_table", "SELECT * FROM users WHERE id = 1", mysql.ComQuery},
		{"cross_shard_aggregate", "SELECT COUNT(*) FROM orders", mysql.ComQuery},
		{"cross_shard_group_by", "SELECT user_id, SUM(amount) FROM orders GROUP BY user_id", mysql.ComQuery},
		{"cross_shard_distinct", "SELECT DISTINCT user_id FROM orders", mysql.ComQuery},
		{"cross_shard_order_by", "SELECT * FROM orders ORDER BY id", mysql.ComQuery},
		{"cross_shard_limit", "SELECT * FROM orders LIMIT 1", mysql.ComQuery},
		{"cross_shard_window", "SELECT ROW_NUMBER() OVER (ORDER BY id) FROM orders", mysql.ComQuery},
		{"cross_shard_calc_found_rows", "SELECT SQL_CALC_FOUND_ROWS * FROM orders", mysql.ComQuery},
		{"cross_shard_for_update", "SELECT * FROM orders FOR UPDATE", mysql.ComQuery},
		{"cross_shard_into_outfile", "SELECT * FROM orders INTO OUTFILE '/tmp/orders.txt'", mysql.ComQuery},
		{"cross_shard_in_order_by", "SELECT * FROM orders WHERE user_id IN (1, 2) ORDER BY id", mysql.ComQuery},
		{"cross_shard_in_aggregate", "SELECT COUNT(*) FROM orders WHERE user_id IN (1, 2)", mysql.ComQuery},
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
			var rej *errs.MySQL
			require.ErrorAs(t, err, &rej)
			require.Equal(t, uint16(1235), rej.Number)
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
	var rej *errs.MySQL
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
		{"empty_slice_name", []shard.Rule{{Table: "t", Key: "id", Algorithm: constant.ShardingHashMod, Slices: []string{""}}}},
		{"duplicate_slice", []shard.Rule{{Table: "t", Key: "id", Algorithm: constant.ShardingHashMod, Slices: []string{"s0", "s0"}}}},
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
