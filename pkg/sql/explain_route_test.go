package sql_test

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/sql"
	"github.com/stretchr/testify/require"
)

func TestParseExplainRoute(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		inner string
		ok    bool
	}{
		{"basic", "EXPLAIN ROUTE SELECT 1", "SELECT 1", true},
		{"lower", "explain route select * from t", "select * from t", true},
		{"spaces", "  EXPLAIN   ROUTE   SELECT 1  ", "SELECT 1", true},
		{"comment", "/* hint */ EXPLAIN ROUTE SELECT 1", "SELECT 1", true},
		{"explain_only", "EXPLAIN SELECT 1", "", false},
		{"no_inner", "EXPLAIN ROUTE", "", false},
		{"normal", "SELECT 1", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner, ok := sql.ParseExplainRoute(tc.raw)
			require.Equal(t, tc.ok, ok, tc.raw)
			require.Equal(t, tc.inner, inner)
		})
	}
}
