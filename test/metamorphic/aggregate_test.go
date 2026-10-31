package metamorphic_test

import (
	"context"
	"database/sql"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"

	"github.com/acorndb/shardweave/test/harness"
)

// TestShardedAggregateOracle loads the same logical dataset into a 2-slice
// sharded cluster and a single MySQL oracle, then compares aggregate results
// produced by the proxy with the single-DB oracle.
func TestShardedAggregateOracle(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过需要 Docker 的 metamorphic 测试")
	}
	harness.SkipIfNoDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	tp, err := harness.StartShardedTopology(ctx, 2)
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

	const ddl = `CREATE TABLE orders (
		id INT NOT NULL PRIMARY KEY,
		user_id INT NOT NULL,
		amount INT NULL
	)`
	for _, s := range tp.Slices {
		_, err := s.DB.ExecContext(ctx, ddl)
		require.NoError(t, err)
	}
	_, err = oracle.DB.ExecContext(ctx, ddl)
	require.NoError(t, err)

	type row struct {
		id, userID int
		amount     *int
	}
	var rows []row
	for i := 1; i <= 24; i++ {
		userID := (i*7)%11 + 1
		var amount *int
		if i%7 != 0 {
			v := i*3 - 15
			amount = &v
		}
		rows = append(rows, row{id: i, userID: userID, amount: amount})
	}

	for _, r := range rows {
		_, err := oracle.DB.ExecContext(ctx,
			"INSERT INTO orders (id, user_id, amount) VALUES (?, ?, ?)", r.id, r.userID, r.amount)
		require.NoError(t, err)
		slice := r.userID % 2 // hash_mod over [s0, s1]
		_, err = tp.Slices[slice].DB.ExecContext(ctx,
			"INSERT INTO orders (id, user_id, amount) VALUES (?, ?, ?)", r.id, r.userID, r.amount)
		require.NoError(t, err)
	}

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

	queries := []struct {
		query     string
		unordered bool
	}{
		{query: "SELECT COUNT(*) FROM orders"},
		{query: "SELECT COUNT(amount) FROM orders"},
		{query: "SELECT SUM(amount) FROM orders"},
		{query: "SELECT MIN(amount), MAX(amount) FROM orders"},
		{query: "SELECT COUNT(*), COUNT(amount), SUM(amount), MIN(amount), MAX(amount) FROM orders"},
		{query: "SELECT COUNT(*), SUM(amount) FROM orders WHERE user_id IN (1, 2, 3, 4)"},
		{query: "SELECT COUNT(*), SUM(amount) FROM orders WHERE amount > 0"},
		{query: "SELECT COUNT(*), SUM(amount) FROM orders WHERE user_id > 5"},
		{query: "SELECT AVG(amount) FROM orders"},
		{query: "SELECT COUNT(*), AVG(amount) FROM orders"},
		{query: "SELECT COUNT(*), AVG(amount) FROM orders WHERE user_id IN (1, 2, 3, 4)"},
		{query: "SELECT AVG(amount) FROM orders WHERE amount > 0"},
		{query: "SELECT amount FROM orders ORDER BY user_id, id"},
		{query: "SELECT amount FROM orders ORDER BY user_id DESC, id DESC"},
		{query: "SELECT amount FROM orders ORDER BY user_id * 2 + id"},
		{query: "SELECT amount FROM orders ORDER BY amount % 7, id"},
		{query: "SELECT user_id, COUNT(*), SUM(amount), MIN(amount), MAX(amount), AVG(amount) FROM orders GROUP BY user_id", unordered: true},
		{query: "SELECT amount, COUNT(*), SUM(user_id) FROM orders GROUP BY amount", unordered: true},
		{query: "SELECT user_id, COUNT(*) FROM orders WHERE amount > 0 GROUP BY user_id", unordered: true},
		{query: "SELECT user_id FROM orders GROUP BY user_id", unordered: true},
		{query: "SELECT DISTINCT user_id, amount FROM orders", unordered: true},
		{query: "SELECT DISTINCT amount FROM orders", unordered: true},
		{query: "SELECT DISTINCT user_id FROM orders", unordered: true},
	}
	for _, tc := range queries {
		t.Run(tc.query, func(t *testing.T) {
			proxyCols, proxyRows := queryRows(t, ctx, pc, tc.query)
			oracleCols, oracleRows := queryRows(t, ctx, oracle.DB, tc.query)
			require.Equal(t, oracleCols, proxyCols)
			if tc.unordered {
				sortRows(proxyRows)
				sortRows(oracleRows)
			}
			require.Len(t, proxyRows, len(oracleRows))
			for i := range oracleRows {
				require.Len(t, proxyRows[i], len(oracleRows[i]))
				for j := range oracleRows[i] {
					require.Truef(t, sameCell(oracleRows[i][j], proxyRows[i][j]),
						"row %d col %d: oracle=%+v proxy=%+v", i, j, oracleRows[i][j], proxyRows[i][j])
				}
			}
		})
	}
}

func sortRows(rows [][]sql.NullString) {
	sort.Slice(rows, func(i, j int) bool {
		return rowKey(rows[i]) < rowKey(rows[j])
	})
}

func rowKey(row []sql.NullString) string {
	var b strings.Builder
	for _, c := range row {
		if !c.Valid {
			b.WriteString("N|")
			continue
		}
		b.WriteString(strconv.Itoa(len(c.String)))
		b.WriteByte(':')
		b.WriteString(c.String)
		b.WriteByte('|')
	}
	return b.String()
}

type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryRows(t *testing.T, ctx context.Context, q querier, query string) ([]string, [][]sql.NullString) {
	t.Helper()
	rows, err := q.QueryContext(ctx, query)
	require.NoError(t, err)
	defer rows.Close()

	cols, err := rows.Columns()
	require.NoError(t, err)
	var out [][]sql.NullString
	for rows.Next() {
		cells := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		out = append(out, cells)
	}
	require.NoError(t, rows.Err())
	return cols, out
}

func sameCell(a, b sql.NullString) bool {
	if a.Valid != b.Valid {
		return false
	}
	if !a.Valid {
		return true
	}
	ar, aok := new(big.Rat).SetString(a.String)
	br, bok := new(big.Rat).SetString(b.String)
	if aok && bok {
		return ar.Cmp(br) == 0
	}
	return a.String == b.String
}
