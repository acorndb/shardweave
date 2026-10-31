package sql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnsupportedReason(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string // empty means supported
	}{
		{"show_warnings", "SHOW WARNINGS", "SHOW WARNINGS"},
		{"show_warnings_lower", "show warnings", "SHOW WARNINGS"},
		{"show_warnings_comment", "/* hint */ SHOW WARNINGS LIMIT 10", "SHOW WARNINGS"},
		{"show_errors", "SHOW ERRORS", "SHOW ERRORS"},
		{"show_count_warnings", "SHOW COUNT(*) WARNINGS", "WARNINGS"},
		{"show_count_errors", "SHOW COUNT(*) ERRORS", "ERRORS"},
		{"show_master_status", "SHOW MASTER STATUS", "SHOW MASTER STATUS"},
		{"show_binary_logs", "SHOW BINARY LOGS", "BINARY LOGS"},
		{"show_binary_log_status", "SHOW BINARY LOG STATUS", "BINARY LOG"},
		{"show_slave_status", "SHOW SLAVE STATUS", "SLAVE STATUS"},
		{"show_replica_status", "SHOW REPLICA STATUS", "REPLICA STATUS"},
		{"set_sql_mode", "SET sql_mode = 'ANSI_QUOTES'", "sql_mode"},
		{"set_session_sql_mode", "SET SESSION sql_mode = ''", "sql_mode"},
		{"set_at_at_sql_mode", "SET @@SESSION.sql_mode = ''", "sql_mode"},
		{"set_global_sql_mode", "SET GLOBAL sql_mode = ''", "sql_mode"},
		{"set_at_at_global_sql_mode", "SET @@GLOBAL.sql_mode = ''", "sql_mode"},
		{"set_local_sql_mode", "SET LOCAL sql_mode = ''", "sql_mode"},
		{"set_backtick_sql_mode", "SET `sql_mode` = ''", "sql_mode"},
		{"set_sql_mode_assign_colon", "SET sql_mode := ''", "sql_mode"},
		{"set_multi_assignment", "SET autocommit = 1, sql_mode = ''", "sql_mode"},
		{"set_sql_mode_literal", "SET @x = 'sql_mode'", ""},
		{"found_rows", "SELECT FOUND_ROWS()", "FOUND_ROWS"},
		{"calc_found_rows", "SELECT SQL_CALC_FOUND_ROWS id FROM t LIMIT 1", "SQL_CALC_FOUND_ROWS"},
		{"last_insert_id_expression", "SELECT LAST_INSERT_ID() + 1", "LAST_INSERT_ID"},
		{"last_insert_id_where", "SELECT * FROM t WHERE id = LAST_INSERT_ID()", "LAST_INSERT_ID"},
		{"row_count_alias", "SELECT ROW_COUNT() AS n", "ROW_COUNT"},
		{"user_var_last_insert_id", "SET @x = LAST_INSERT_ID()", "LAST_INSERT_ID"},
		{"string_literal_function", "SELECT 'LAST_INSERT_ID()'", ""},
		{"comment_function", "SELECT 1 /* ROW_COUNT() */", ""},
		{"rewritten_alias", "SELECT CAST(42 AS UNSIGNED) AS `LAST_INSERT_ID()`", ""},
		{"select", "SELECT 1", ""},
		{"show_tables", "SHOW TABLES", ""},
		{"show_variables", "SHOW VARIABLES LIKE 'sql_mode'", ""},
		{"set_names", "SET NAMES utf8mb4", ""},
		{"set_autocommit", "SET autocommit = 0", ""},
		{"set_user_var", "SET @x = 1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := UnsupportedReason(tc.query)
			if tc.want == "" {
				require.Empty(t, reason, tc.query)
				return
			}
			require.Contains(t, reason, tc.want, tc.query)
		})
	}
}
