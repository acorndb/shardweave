package source_test

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/route/source"
	"github.com/stretchr/testify/require"
)

func TestClassify_CorpusB(t *testing.T) {
	// design §5.11 B* — Layer B only
	cases := []struct {
		id   string
		sql  string
		want bool // wantSlave
		ref  string
	}{
		{"B1", "INSERT INTO t VALUES (1)", false, "kingshard handleExec fromSlave=false"},
		{"B2", "UPDATE t SET a=1", false, "kingshard handleExec"},
		{"B3", "DELETE FROM t", false, "kingshard handleExec"},
		{"B4", "REPLACE INTO t VALUES (1)", false, "kingshard handleExec"},
		{"B5", "TRUNCATE t", false, "kingshard handleExec"},
		{"B6", "CREATE TABLE t(i INT)", false, "Gaea non-Select/Show → master"},
		{"B7", "SET NAMES utf8mb4", false, "M4"},
		{"B8", "SET autocommit=0", false, "M4"},
		{"B9", "USE db1", true, "kingshard handleUseDB fromSlave=true"},
		{"B10", "SELECT * FROM t", true, "kingshard handleSelect default"},
		{"B11", "SELECT * FROM t FOR UPDATE", false, "Gaea checkExecuteFromSlave"},
		{"B12", "SELECT * FROM t LOCK IN SHARE MODE", false, "Gaea checkExecuteFromSlave"},
		{"B13", "SELECT /*master*/ * FROM t", false, "kingshard MasterComment / Gaea *master*"},
		{"B14", "/*master*/ SELECT * FROM t", false, "Gaea master hint leading"},
		{"B15", "SHOW TABLES", true, "Gaea StmtShow"},
		{"B16", "SHOW VARIABLES LIKE '%read_only%'", false, "Gaea readonlyVariable"},
		{"B17", "SELECT @@read_only", false, "Gaea @@read_only"},
		{"B18", "EXPLAIN SELECT * FROM t", true, "S3"},
		{"B19", "SELECT * FROM t; DELETE FROM t", false, "§5.10.1 multi-stmt"},
		{"B20", "-- c\nSELECT 1", true, "comment strip"},
		{"B21", "select * from t", true, "case"},
		{"B22", "-- only", false, "M9 empty"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			got := source.ClassifyWantSlave(tc.sql)
			require.Equal(t, tc.want, got, "%s ref=%s sql=%q", tc.id, tc.ref, tc.sql)
		})
	}
}
