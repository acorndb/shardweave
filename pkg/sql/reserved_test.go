package sql_test

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/sql"
	"github.com/stretchr/testify/require"
)

func TestDetectReservedSQL(t *testing.T) {
	cases := []struct {
		raw  string
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
		t.Run(tc.raw, func(t *testing.T) {
			require.Equal(t, tc.want, sql.DetectReservedSQL(tc.raw))
		})
	}
}
