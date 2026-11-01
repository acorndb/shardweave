package metamorphic_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"

	"github.com/acorndb/shardweave/test/harness"
)

// TestShardedCollationOracle is the D5 end-to-end gate for string collation
// merging: the same logical dataset is loaded into a 4-slice sharded cluster
// and into a single MySQL oracle, then ORDER BY / GROUP BY / DISTINCT /
// aggregate results produced through the proxy are compared with the oracle.
//
// Data is designed to distinguish PAD SPACE from NO PAD and case-sensitive
// from case-insensitive collations:
//
//	general_ci (PAD, CI):  3 groups: {a,a ,A} {b,B} {c}
//	0900_ai_ci (NO PAD,CI): 4 groups: {a,A} {a } {b,B} {c}
//	utf8mb4_bin (PAD, CS):  5 groups: {a,a } {A} {b} {B} {c}
func TestShardedCollationOracle(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过需要 Docker 的 metamorphic 测试")
	}
	harness.SkipIfNoDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	tp, err := harness.StartShardedTopology(ctx, 4)
	require.NoError(t, err)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer closeCancel()
		tp.Close(closeCtx)
	})

	oracleTP, err := harness.StartShardedTopology(ctx, 1)
	require.NoError(t, err)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer closeCancel()
		oracleTP.Close(closeCtx)
	})
	oracle := oracleTP.Slices[0]

	proxy, err := harness.StartProxy(tp.ProxyConfig())
	require.NoError(t, err)
	t.Cleanup(proxy.Close)
	conn, err := sql.Open("mysql", proxy.DSN())
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = conn.Close() })
	pc, err := conn.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })

	cases := []struct {
		collation  string
		wantGroups int
	}{
		{"utf8mb4_general_ci", 3},
		{"utf8mb4_0900_ai_ci", 4},
		{"utf8mb4_bin", 5},
	}

	for _, tc := range cases {
		t.Run(tc.collation, func(t *testing.T) {
			ddl := fmt.Sprintf(`CREATE TABLE orders (
				id INT NOT NULL PRIMARY KEY,
				user_id INT NOT NULL,
				name_grp VARCHAR(32) CHARACTER SET utf8mb4 COLLATE %s NOT NULL,
				name_ord VARCHAR(32) CHARACTER SET utf8mb4 COLLATE %s NOT NULL
			)`, tc.collation, tc.collation)
			execAll := func(query string, args ...any) {
				t.Helper()
				for _, s := range tp.Slices {
					_, err := s.DB.ExecContext(ctx, query, args...)
					require.NoError(t, err)
				}
				_, err := oracle.DB.ExecContext(ctx, query, args...)
				require.NoError(t, err)
			}
			execAll("DROP TABLE IF EXISTS orders")
			execAll(ddl)

			grpVals := []string{"a", "a ", "A", "b", "B", "c"}
			ordVals := []string{"ant", "Bee", "cat", "Dog"}
			const insert = "INSERT INTO orders (id, user_id, name_grp, name_ord) VALUES (?, ?, ?, ?)"
			for i := 1; i <= 48; i++ {
				userID := i%4 + 1
				grp := grpVals[i%len(grpVals)]
				ord := ordVals[i%len(ordVals)]
				_, err := oracle.DB.ExecContext(ctx, insert, i, userID, grp, ord)
				require.NoError(t, err)
				_, err = tp.Slices[userID%4].DB.ExecContext(ctx, insert, i, userID, grp, ord)
				require.NoError(t, err)
			}
			for _, s := range tp.Slices {
				var n int
				require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM orders").Scan(&n))
				require.Greater(t, n, 0, "every configured slice must hold data")
			}

			// ORDER BY: id breaks ties deterministically, so the full sequence
			// must match the oracle exactly.
			orderQ := "SELECT id, name_ord FROM orders ORDER BY name_ord, id"
			requireFourShardRoute(t, ctx, pc, "EXPLAIN ROUTE "+orderQ)
			pCols, pRows := queryRows(t, ctx, pc, orderQ)
			oCols, oRows := queryRows(t, ctx, oracle.DB, orderQ)
			requireSameRows(t, orderQ, pCols, pRows, oCols, oRows)

			// GROUP BY: the representative value of a group is not deterministic
			// in MySQL 8, so compare the multiset of group sizes.
			groupQ := "SELECT name_grp, COUNT(*) FROM orders GROUP BY name_grp"
			requireFourShardRoute(t, ctx, pc, "EXPLAIN ROUTE "+groupQ)
			_, pgRows := queryRows(t, ctx, pc, groupQ)
			_, ogRows := queryRows(t, ctx, oracle.DB, groupQ)
			require.Len(t, ogRows, tc.wantGroups, "oracle group count (test data sanity)")
			require.Equal(t, sortedCounts(t, ogRows, groupQ), sortedCounts(t, pgRows, groupQ),
				"%s: group sizes through the proxy must match the oracle", groupQ)

			// DISTINCT: equivalence classes are deterministic; the chosen
			// representative is not, so compare the class count.
			distinctQ := "SELECT DISTINCT name_grp FROM orders"
			_, pdRows := queryRows(t, ctx, pc, distinctQ)
			_, odRows := queryRows(t, ctx, oracle.DB, distinctQ)
			require.Len(t, odRows, tc.wantGroups, "oracle distinct count (test data sanity)")
			require.Len(t, pdRows, tc.wantGroups, "proxy distinct count must match the oracle")

			countDistinctQ := "SELECT COUNT(DISTINCT name_grp) FROM orders"
			pCols, pRows = queryRows(t, ctx, pc, countDistinctQ)
			oCols, oRows = queryRows(t, ctx, oracle.DB, countDistinctQ)
			requireSameRows(t, countDistinctQ, pCols, pRows, oCols, oRows)

			// MIN/MAX: name_ord values are distinct per collation, so the
			// result is deterministic and exercises collation ordering.
			minMaxQ := "SELECT MIN(name_ord), MAX(name_ord) FROM orders"
			pCols, pRows = queryRows(t, ctx, pc, minMaxQ)
			oCols, oRows = queryRows(t, ctx, oracle.DB, minMaxQ)
			requireSameRows(t, minMaxQ, pCols, pRows, oCols, oRows)
		})
	}
}

// sortedCounts returns the COUNT(*) column sorted as strings.
func sortedCounts(t *testing.T, rows [][]sql.NullString, query string) []string {
	t.Helper()
	out := make([]string, len(rows))
	for i, row := range rows {
		require.Greater(t, len(row), 1, query)
		out[i] = row[1].String
	}
	sort.Strings(out)
	return out
}
