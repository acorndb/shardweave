package sqlmatrix_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/route/shard"
	"github.com/acorndb/shardweave/pkg/route/source"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
	sqlparser "github.com/acorndb/shardweave/pkg/sql/parser"
	"github.com/acorndb/shardweave/test/sqlmatrix"
)

// TestSQLSupportMatrix is the plan-level gate: it pins what every syntax family
// does in single/rwsplit (raw forwarding) and sharding mode. Execution may
// impose further constraints in the merge layer; those are covered by
// test/sharding and listed in docs/architecture/sql-support-matrix.md.
func TestSQLSupportMatrix(t *testing.T) {
	cases, err := sqlmatrix.Load("matrix.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, cases)

	router, err := shard.New([]shard.Rule{
		{Table: "orders", Key: "user_id", Algorithm: "hash_mod", Slices: []string{"s0", "s1", "s2", "s3"}},
		{Table: "profiles", Key: "name", Algorithm: "hash_mod_str", Slices: []string{"s0", "s1", "s2", "s3"}},
		{Table: "sql_type_test", Key: "shard_key", Algorithm: "hash_mod", Slices: []string{"s0", "s1", "s2", "s3"}},
	})
	require.NoError(t, err)

	for i, c := range cases {
		c := c
		t.Run(fmt.Sprintf("%s/%02d", c.Category, i), func(t *testing.T) {
			parsed, err := sqlparser.Parse(c.SQL)
			require.NoErrorf(t, err, "parse %q", c.SQL)
			require.Lenf(t, parsed.Stmts, 1, "parse %q", c.SQL)

			stmt := &sql.Statement{Cmd: mysql.ComQuery, Raw: c.SQL, AST: parsed.Stmts[0]}
			stmt.EnsureClass()

			single, err := (plan.Builder{Router: source.AllMaster{Slice: "default"}}).Build(
				context.Background(), session.NewState(), stmt)
			assertPlan(t, "single/rwsplit", c.SQL, single, err, c.Single)

			sharded, err := (plan.Builder{Router: router, Sharding: true}).Build(
				context.Background(), session.NewState(), stmt)
			assertPlan(t, "sharding", c.SQL, sharded, err, c.Sharding)
		})
	}
}

func assertPlan(t *testing.T, mode, sqlText string, p *plan.Plan, err error, want sqlmatrix.Expect) {
	t.Helper()
	kind, reason := outcome(p, err)
	require.Equalf(t, want.Kind, kind, "%s: %s (reason=%q)", mode, sqlText, reason)
	if want.Reason != "" {
		require.Containsf(t, reason, want.Reason, "%s: %s", mode, sqlText)
	}
}

func outcome(p *plan.Plan, err error) (string, string) {
	switch {
	case err != nil:
		return "reject", err.Error()
	case p == nil:
		return "reject", "nil plan"
	case p.Kind == plan.Reject:
		return "reject", p.Reject.Message
	case p.Kind == plan.Passthrough:
		return "passthrough", ""
	case p.Kind == plan.SingleShard:
		return "single_shard", ""
	case p.Kind == plan.MultiShard:
		return "multi_shard", ""
	default:
		return "unknown", ""
	}
}
