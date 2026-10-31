package metamorphic_test

import (
	"context"
	"database/sql"
	"math/big"
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

	queries := []string{
		"SELECT COUNT(*) FROM orders",
		"SELECT COUNT(amount) FROM orders",
		"SELECT SUM(amount) FROM orders",
		"SELECT MIN(amount), MAX(amount) FROM orders",
		"SELECT COUNT(*), COUNT(amount), SUM(amount), MIN(amount), MAX(amount) FROM orders",
		"SELECT COUNT(*), SUM(amount) FROM orders WHERE user_id IN (1, 2, 3, 4)",
		"SELECT COUNT(*), SUM(amount) FROM orders WHERE amount > 0",
		"SELECT COUNT(*), SUM(amount) FROM orders WHERE user_id > 5",
		"SELECT AVG(amount) FROM orders",
		"SELECT COUNT(*), AVG(amount) FROM orders",
		"SELECT COUNT(*), AVG(amount) FROM orders WHERE user_id IN (1, 2, 3, 4)",
		"SELECT AVG(amount) FROM orders WHERE amount > 0",
		"SELECT amount FROM orders ORDER BY user_id, id",
		"SELECT amount FROM orders ORDER BY user_id DESC, id DESC",
	}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			proxyCols, proxyRows := queryRows(t, ctx, pc, query)
			oracleCols, oracleRows := queryRows(t, ctx, oracle.DB, query)
			require.Equal(t, oracleCols, proxyCols)
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
