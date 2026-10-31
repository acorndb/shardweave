package sharding_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"

	"github.com/acorndb/shardweave/test/harness"
)

func TestSingleShardRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过需要 Docker 的分片测试")
	}
	harness.SkipIfNoDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	tp, err := harness.StartShardedTopology(ctx, 2)
	require.NoError(t, err)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer closeCancel()
		tp.Close(closeCtx)
	})

	for _, s := range tp.Slices {
		_, err := s.DB.ExecContext(ctx, `CREATE TABLE orders (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			user_id INT NOT NULL,
			amount INT NOT NULL
		)`)
		require.NoError(t, err)
	}

	proxy, err := harness.StartProxy(tp.ProxyConfig())
	require.NoError(t, err)
	t.Cleanup(proxy.Close)

	conn := openConn(t, ctx, proxy.DSN())

	// user_id=2 -> even -> s0.
	_, err = conn.ExecContext(ctx, "INSERT INTO orders (user_id, amount) VALUES (2, 100)")
	require.NoError(t, err)
	require.Equal(t, 1, shardCount(t, ctx, tp.Slices[0], "SELECT COUNT(*) FROM orders WHERE user_id = 2"))
	require.Equal(t, 0, shardCount(t, ctx, tp.Slices[1], "SELECT COUNT(*) FROM orders WHERE user_id = 2"))

	var id, amount int
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT id, amount FROM orders WHERE user_id = 2").Scan(&id, &amount))
	require.Equal(t, 100, amount)
	require.Equal(t, 1, id)
	require.Equal(t, 1, connScalar(t, ctx, conn, "SELECT LAST_INSERT_ID()"))

	// user_id=1 -> odd -> s1.
	_, err = conn.ExecContext(ctx, "INSERT INTO orders (user_id, amount) VALUES (1, 50)")
	require.NoError(t, err)
	require.Equal(t, 1, shardCount(t, ctx, tp.Slices[1], "SELECT COUNT(*) FROM orders WHERE user_id = 1"))
	require.Equal(t, 1, shardCount(t, ctx, tp.Slices[0], "SELECT COUNT(*) FROM orders"))

	// UPDATE/DELETE stay on the shard selected by the key.
	_, err = conn.ExecContext(ctx, "UPDATE orders SET amount = 200 WHERE user_id = 2")
	require.NoError(t, err)
	require.Equal(t, 200, shardCount(t, ctx, tp.Slices[0], "SELECT amount FROM orders WHERE user_id = 2"))
	require.Equal(t, 1, connScalar(t, ctx, conn, "SELECT ROW_COUNT()"))
	require.Equal(t, -1, connScalar(t, ctx, conn, "SELECT ROW_COUNT()"))

	_, err = conn.ExecContext(ctx, "DELETE FROM orders WHERE user_id = 1")
	require.NoError(t, err)
	require.Equal(t, 0, shardCount(t, ctx, tp.Slices[1], "SELECT COUNT(*) FROM orders WHERE user_id = 1"))

	require.Equal(t, 1, connScalar(t, ctx, conn, "SELECT 1"))

	assertRoute(t, ctx, conn, "EXPLAIN ROUTE SELECT amount FROM orders WHERE user_id = 2",
		"SingleShard", "s0", "slave", "")
	assertRoute(t, ctx, conn, "EXPLAIN ROUTE SELECT * FROM orders",
		"Reject", "", "", "WHERE")

	// Unsupported shapes are explicit 1235, not silent wrong results.
	assertReject(t, ctx, conn, "SELECT * FROM orders WHERE user_id IN (1, 2)")
	assertReject(t, ctx, conn, "SELECT * FROM orders")
	assertReject(t, ctx, conn, "SELECT * FROM users WHERE id = 1")
	assertReject(t, ctx, conn, "CREATE TABLE t (id INT)")
	assertReject(t, ctx, conn, "BEGIN")

	db := openDB(t, proxy.DSN())
	_, err = db.PrepareContext(ctx, "SELECT amount FROM orders WHERE user_id = ?")
	var me *drivermysql.MySQLError
	require.ErrorAs(t, err, &me)
	require.Equal(t, uint16(1235), me.Number)
}

func openConn(t *testing.T, ctx context.Context, dsn string) *sql.Conn {
	t.Helper()
	db := openDB(t, dsn)
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func shardCount(t *testing.T, ctx context.Context, s *harness.Instance, query string) int {
	t.Helper()
	var n int
	require.NoErrorf(t, s.DB.QueryRowContext(ctx, query).Scan(&n), "查询失败: %s", query)
	return n
}

func connScalar(t *testing.T, ctx context.Context, conn *sql.Conn, query string) int {
	t.Helper()
	var n int
	require.NoErrorf(t, conn.QueryRowContext(ctx, query).Scan(&n), "查询失败: %s", query)
	return n
}

func assertReject(t *testing.T, ctx context.Context, conn *sql.Conn, query string) {
	t.Helper()
	_, err := conn.ExecContext(ctx, query)
	var me *drivermysql.MySQLError
	require.ErrorAsf(t, err, &me, "期望 1235: %s", query)
	require.Equal(t, uint16(1235), me.Number, query)
}

func assertRoute(t *testing.T, ctx context.Context, conn *sql.Conn, query, kind, slice, role, reasonPart string) {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query)
	require.NoError(t, err)
	defer rows.Close()

	cols, err := rows.Columns()
	require.NoError(t, err)
	require.Equal(t, []string{"kind", "slice", "role", "db", "sql", "reason"}, cols)
	require.True(t, rows.Next())

	var gotKind, gotSlice, gotRole, db, sqlText, reason string
	require.NoError(t, rows.Scan(&gotKind, &gotSlice, &gotRole, &db, &sqlText, &reason))
	require.Equal(t, kind, gotKind)
	require.Equal(t, slice, gotSlice)
	require.Equal(t, role, gotRole)
	if reasonPart != "" {
		require.Contains(t, reason, reasonPart)
	}
}
