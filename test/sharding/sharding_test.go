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
		_, err = s.DB.ExecContext(ctx, `CREATE TABLE profiles (
			name VARCHAR(32) NOT NULL PRIMARY KEY,
			score INT NOT NULL
		)`)
		require.NoError(t, err)
	}

	proxy, err := harness.StartProxy(tp.ProxyConfig())
	require.NoError(t, err)
	t.Cleanup(proxy.Close)

	conn := openConn(t, ctx, proxy.DSN())

	// Connection/session compatibility: metadata reads and session statements
	// must not be rejected just because they have no shard key.
	requireQueryHasRow(t, ctx, conn, "SHOW TABLES")
	requireQueryHasRow(t, ctx, conn, "SHOW COLUMNS FROM orders")
	requireQueryHasRow(t, ctx, conn, "SHOW CREATE TABLE orders")
	requireQueryHasRow(t, ctx, conn, "SHOW INDEX FROM orders")
	requireQueryHasRow(t, ctx, conn, "DESC orders")
	_, err = conn.ExecContext(ctx, "USE bench")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "SET NAMES utf8mb4")
	require.NoError(t, err)
	require.Equal(t, "bench", connScalarString(t, ctx, conn, "SELECT DATABASE()"))
	assertReject(t, ctx, conn, "SHOW MASTER STATUS")
	assertReject(t, ctx, conn, "SHOW STATUS")
	assertReject(t, ctx, conn, "SET sql_mode = 'ANSI_QUOTES'")

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

	assertRoute(t, ctx, conn, "EXPLAIN ROUTE SELECT amount FROM orders WHERE user_id = 2",
		"SingleShard", "s0")
	assertRoute(t, ctx, conn, "EXPLAIN ROUTE SELECT * FROM orders",
		"MultiShard", "s0")
	requireRouteSlices(t, ctx, conn, "EXPLAIN ROUTE SELECT * FROM orders WHERE user_id IN (1, 2)",
		[]string{"s0", "s1"})

	// Prepared SELECT: the shard key must be a literal at prepare time; other
	// parameters are supported by the backend protocol.
	prep, err := conn.PrepareContext(ctx,
		"SELECT amount FROM orders WHERE user_id = 2 AND amount > ?")
	require.NoError(t, err)
	var preparedAmount int
	require.NoError(t, prep.QueryRowContext(ctx, 0).Scan(&preparedAmount))
	require.Equal(t, 200, preparedAmount)
	require.NoError(t, prep.Close())

	_, err = conn.PrepareContext(ctx, "SELECT amount FROM orders WHERE user_id = ?")
	var prepErr *drivermysql.MySQLError
	require.ErrorAs(t, err, &prepErr)
	require.Equal(t, uint16(1235), prepErr.Number, "parameterized shard key is not routable")

	_, err = conn.PrepareContext(ctx, "UPDATE orders SET amount = ? WHERE user_id = 2")
	require.ErrorAs(t, err, &prepErr)
	require.Equal(t, uint16(1235), prepErr.Number, "prepared DML is not supported")

	_, err = conn.ExecContext(ctx, "DELETE FROM orders WHERE user_id = 1")
	require.NoError(t, err)
	require.Equal(t, 0, shardCount(t, ctx, tp.Slices[1], "SELECT COUNT(*) FROM orders WHERE user_id = 1"))

	require.Equal(t, 1, connScalar(t, ctx, conn, "SELECT 1"))

	// Multi-shard SELECT scatters to every owning slice and concatenates the
	// per-shard result sets. Rows are currently returned in slice order, not
	// sorted, so order-insensitive assertions are used.
	_, err = conn.ExecContext(ctx, "INSERT INTO orders (user_id, amount) VALUES (1, 50)")
	require.NoError(t, err)
	require.Equal(t, 1, shardCount(t, ctx, tp.Slices[1], "SELECT COUNT(*) FROM orders WHERE user_id = 1"))

	requireShardRows(t, ctx, conn,
		"SELECT user_id, amount FROM orders WHERE user_id IN (1, 2)",
		map[int]int{1: 50, 2: 200})
	requireShardRows(t, ctx, conn,
		"SELECT user_id, amount FROM orders",
		map[int]int{1: 50, 2: 200})
	requireColumns(t, ctx, conn, "SELECT id, user_id, amount FROM orders",
		[]string{"id", "user_id", "amount"})

	// LIMIT without ORDER BY is safe to apply after concat: each shard applies
	// the same LIMIT and the merge truncates to the final count.
	requireRowCount(t, ctx, conn, "SELECT user_id, amount FROM orders LIMIT 1", 1)
	requireRowCount(t, ctx, conn, "SELECT user_id, amount FROM orders LIMIT 0", 0)
	requireRowCount(t, ctx, conn, "SELECT user_id, amount FROM orders WHERE user_id IN (1, 2) LIMIT 1", 1)
	requireRowCount(t, ctx, conn, "SELECT user_id, amount FROM orders LIMIT 100", 2)
	require.Equal(t, 1, connScalar(t, ctx, conn, "SELECT 1"), "limit must not break the session")
	requireRowCount(t, ctx, conn, "SELECT user_id FROM orders LIMIT 5 OFFSET 10", 0)

	// ORDER BY is executed per shard and merged with a k-way heap.
	requireOrderedColumn(t, ctx, conn, "SELECT user_id FROM orders ORDER BY user_id",
		[]string{"1", "2"})
	requireOrderedColumn(t, ctx, conn, "SELECT user_id FROM orders ORDER BY user_id DESC",
		[]string{"2", "1"})
	requireOrderedColumn(t, ctx, conn, "SELECT user_id FROM orders ORDER BY 1 DESC",
		[]string{"2", "1"})
	requireOrderedColumn(t, ctx, conn, "SELECT user_id FROM orders ORDER BY user_id LIMIT 1",
		[]string{"1"})
	requireOrderedColumn(t, ctx, conn, "SELECT user_id FROM orders ORDER BY user_id LIMIT 1 OFFSET 1",
		[]string{"2"})
	requireOrderedColumn(t, ctx, conn, "SELECT amount FROM orders ORDER BY user_id",
		[]string{"50", "200"})
	requireOrderedColumn(t, ctx, conn, "SELECT amount FROM orders ORDER BY user_id DESC",
		[]string{"200", "50"})
	requireOrderedColumn(t, ctx, conn, "SELECT amount FROM orders ORDER BY user_id LIMIT 1 OFFSET 1",
		[]string{"200"})
	requireOrderedColumn(t, ctx, conn, "SELECT amount FROM orders ORDER BY amount + 1",
		[]string{"50", "200"})
	requireOrderedColumn(t, ctx, conn, "SELECT amount FROM orders ORDER BY -amount",
		[]string{"200", "50"})
	requireOrderedColumn(t, ctx, conn, "SELECT amount FROM orders ORDER BY amount % 100",
		[]string{"200", "50"})
	assertReject(t, ctx, conn, "SELECT * FROM orders ORDER BY amount + 1")
	// String ORDER BY needs collation-aware comparison; reject instead of
	// returning a byte-order result that may differ from MySQL.
	assertReject(t, ctx, conn, "SELECT name FROM profiles ORDER BY name")

	// Aggregates without GROUP BY: each shard returns one partial row and the
	// merge combines COUNT/SUM/MIN/MAX.
	var count, sum, min, max int
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT COUNT(*), SUM(amount), MIN(amount), MAX(amount) FROM orders").
		Scan(&count, &sum, &min, &max))
	require.Equal(t, []int{2, 250, 50, 200}, []int{count, sum, min, max})
	var avg float64
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT AVG(amount) FROM orders").Scan(&avg))
	require.Equal(t, 125.0, avg)
	var count2 int
	var avg2 float64
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*), AVG(amount) FROM orders").
		Scan(&count2, &avg2))
	require.Equal(t, 2, count2)
	require.Equal(t, 125.0, avg2)
	require.Equal(t, 2, connScalar(t, ctx, conn, "SELECT COUNT(*) FROM orders WHERE user_id IN (1, 2)"))
	requireRowCount(t, ctx, conn, "SELECT COUNT(*) FROM orders LIMIT 1", 1)
	requireRowCount(t, ctx, conn, "SELECT COUNT(*) FROM orders LIMIT 0", 0)
	require.Equal(t, 1, connScalar(t, ctx, conn, "SELECT 1"), "aggregate must not break the session")

	requireRowValues(t, ctx, conn,
		"SELECT COUNT(DISTINCT user_id) FROM orders", [][]string{{"2"}})
	requireRowValues(t, ctx, conn,
		"SELECT AVG(DISTINCT amount) FROM orders", [][]string{{"125.0000"}})
	requireRowValues(t, ctx, conn,
		"SELECT MIN(DISTINCT amount), MAX(DISTINCT amount) FROM orders", [][]string{{"50", "200"}})
	requireRowValues(t, ctx, conn,
		"SELECT COUNT(DISTINCT amount + 1), COUNT(DISTINCT 1) FROM orders", [][]string{{"2", "1"}})
	requireRowCount(t, ctx, conn, "SELECT COUNT(DISTINCT amount) FROM orders LIMIT 0", 0)
	requireRowCount(t, ctx, conn, "SELECT COUNT(DISTINCT amount) FROM orders LIMIT 1 OFFSET 1", 0)
	requireRouteSlices(t, ctx, conn, "EXPLAIN ROUTE SELECT COUNT(DISTINCT amount) FROM orders",
		[]string{"s0", "s1"})
	assertReject(t, ctx, conn, "SELECT COUNT(DISTINCT amount, id) FROM orders")
	assertReject(t, ctx, conn, "SELECT COUNT(DISTINCT name) FROM profiles")
	assertReject(t, ctx, conn, "SELECT COUNT(*), amount FROM orders")
	assertReject(t, ctx, conn, "SELECT COUNT(*) FROM orders ORDER BY 1")
	assertReject(t, ctx, conn, "SELECT * FROM orders WHERE user_id IN (SELECT user_id FROM orders)")

	// hash_mod accepts quoted numeric literals as the same integer key.
	var quotedAmount int
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT amount FROM orders WHERE user_id = '2'").Scan(&quotedAmount))
	require.Equal(t, 200, quotedAmount)

	// hash_mod_str: string keys use FNV, and numeric literals are rejected
	// instead of being coerced into a different hash domain.
	_, err = conn.ExecContext(ctx, "INSERT INTO profiles (name, score) VALUES ('alice', 1)")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "INSERT INTO profiles (name, score) VALUES ('bob', 2)")
	require.NoError(t, err)
	require.Equal(t, 1, shardCount(t, ctx, tp.Slices[1], "SELECT COUNT(*) FROM profiles WHERE name = 'alice'"))
	require.Equal(t, 1, shardCount(t, ctx, tp.Slices[0], "SELECT COUNT(*) FROM profiles WHERE name = 'bob'"))
	require.Equal(t, 1, connScalar(t, ctx, conn, "SELECT score FROM profiles WHERE name = 'alice'"))
	requireProfiles(t, ctx, conn, "SELECT name, score FROM profiles",
		map[string]int{"alice": 1, "bob": 2})
	require.Equal(t, 2, connScalar(t, ctx, conn, "SELECT COUNT(*) AS c FROM profiles"))
	requireOrderedColumn(t, ctx, conn, "SELECT name FROM profiles ORDER BY score",
		[]string{"alice", "bob"})
	var profileAvg float64
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT AVG(score) FROM profiles").Scan(&profileAvg))
	require.Equal(t, 1.5, profileAvg)
	assertRoute(t, ctx, conn, "EXPLAIN ROUTE SELECT score FROM profiles WHERE name = 'alice'",
		"SingleShard", "s1")
	requireRouteSlices(t, ctx, conn, "EXPLAIN ROUTE SELECT name, score FROM profiles",
		[]string{"s0", "s1"})
	requireRouteSlices(t, ctx, conn, "EXPLAIN ROUTE SELECT COUNT(*) FROM orders",
		[]string{"s0", "s1"})
	requireRouteSlices(t, ctx, conn, "EXPLAIN ROUTE SELECT amount, COUNT(*) FROM orders GROUP BY amount",
		[]string{"s0", "s1"})
	assertReject(t, ctx, conn, "SELECT score FROM profiles WHERE name = 1")
	assertReject(t, ctx, conn, "INSERT INTO profiles (name, score) VALUES (1, 3)")

	// A live prepared statement pins its slice. Multi-shard statements are
	// rejected until the last statement closes and releases the pin.
	pinned, err := conn.PrepareContext(ctx, "SELECT amount FROM orders WHERE user_id = 2 AND amount > ?")
	require.NoError(t, err)
	_, err = conn.QueryContext(ctx, "SELECT user_id, amount FROM orders")
	var pinnedErr *drivermysql.MySQLError
	require.ErrorAs(t, err, &pinnedErr)
	require.Equal(t, uint16(1235), pinnedErr.Number)
	require.NoError(t, pinned.Close())
	requireShardRows(t, ctx, conn, "SELECT user_id, amount FROM orders",
		map[int]int{1: 50, 2: 200})

	// GROUP BY merges partial groups from each shard. Add a duplicate amount on
	// the other slice so group 50 spans both shards.
	_, err = conn.ExecContext(ctx, "INSERT INTO orders (user_id, amount) VALUES (2, 50)")
	require.NoError(t, err)
	requireGroupRows(t, ctx, conn,
		"SELECT amount, COUNT(*), SUM(user_id) FROM orders GROUP BY amount",
		map[string][]string{
			"50":  {"2", "3"},
			"200": {"1", "2"},
		})
	requireGroupRows(t, ctx, conn,
		"SELECT user_id, COUNT(*), SUM(amount) FROM orders GROUP BY user_id",
		map[string][]string{
			"1": {"1", "50"},
			"2": {"2", "250"},
		})
	requireRowValues(t, ctx, conn,
		"SELECT user_id, COUNT(*) AS c, SUM(amount) AS s FROM orders GROUP BY user_id ORDER BY c DESC, user_id",
		[][]string{{"2", "2", "250"}, {"1", "1", "50"}})
	requireRowValues(t, ctx, conn,
		"SELECT user_id, SUM(amount) AS s FROM orders GROUP BY user_id ORDER BY s DESC LIMIT 1",
		[][]string{{"2", "250"}})
	requireRowCount(t, ctx, conn, "SELECT user_id, COUNT(*) FROM orders GROUP BY user_id LIMIT 1", 1)
	requireRowValues(t, ctx, conn,
		"SELECT amount, COUNT(*) AS c FROM orders GROUP BY amount HAVING c > 1 ORDER BY amount",
		[][]string{{"50", "2"}})
	requireRowValues(t, ctx, conn,
		"SELECT amount, COUNT(*) FROM orders GROUP BY amount HAVING COUNT(*) > 1",
		[][]string{{"50", "2"}})
	requireRowValues(t, ctx, conn,
		"SELECT user_id, SUM(amount) AS s FROM orders GROUP BY user_id HAVING s >= 200 ORDER BY user_id",
		[][]string{{"2", "250"}})
	requireRowValues(t, ctx, conn,
		"SELECT user_id, COUNT(*) FROM orders GROUP BY user_id HAVING COUNT(*) > 1 AND user_id > 1 ORDER BY user_id",
		[][]string{{"2", "2"}})
	requireRowValues(t, ctx, conn,
		"SELECT COUNT(*), COUNT(DISTINCT amount), SUM(DISTINCT amount), AVG(DISTINCT amount) FROM orders",
		[][]string{{"3", "2", "250", "125.0000"}})
	requireGroupRows(t, ctx, conn,
		"SELECT user_id, COUNT(DISTINCT amount) FROM orders GROUP BY user_id",
		map[string][]string{"1": {"1"}, "2": {"2"}})
	requireRowValues(t, ctx, conn,
		"SELECT user_id, COUNT(DISTINCT amount) AS c FROM orders GROUP BY user_id HAVING c > 1",
		[][]string{{"2", "2"}})
	requireRowValues(t, ctx, conn,
		"SELECT user_id, COUNT(DISTINCT amount) AS c FROM orders GROUP BY user_id ORDER BY c DESC LIMIT 1",
		[][]string{{"2", "2"}})
	requireRowValues(t, ctx, conn,
		"SELECT user_id, AVG(amount), COUNT(DISTINCT amount) FROM orders GROUP BY user_id ORDER BY user_id",
		[][]string{{"1", "50.0000", "1"}, {"2", "125.0000", "2"}})
	requireRowValues(t, ctx, conn,
		"SELECT user_id, COUNT(DISTINCT amount) AS c FROM orders GROUP BY user_id ORDER BY 2 DESC, 1",
		[][]string{{"2", "2"}, {"1", "1"}})
	requireRowValues(t, ctx, conn,
		"SELECT user_id, COUNT(DISTINCT amount) AS c, SUM(DISTINCT amount) AS s FROM orders GROUP BY user_id HAVING s > 100 ORDER BY user_id",
		[][]string{{"2", "2", "250"}})
	requireRowValues(t, ctx, conn,
		"SELECT amount, COUNT(DISTINCT user_id) FROM orders GROUP BY amount ORDER BY amount LIMIT 1 OFFSET 1",
		[][]string{{"200", "1"}})
	requireRowValues(t, ctx, conn,
		"SELECT user_id, amount, COUNT(DISTINCT user_id) FROM orders GROUP BY user_id, amount ORDER BY user_id, amount",
		[][]string{{"1", "50", "1"}, {"2", "50", "1"}, {"2", "200", "1"}})
	requireRowCount(t, ctx, conn, "SELECT user_id, COUNT(DISTINCT amount) FROM orders GROUP BY user_id LIMIT 0", 0)
	assertReject(t, ctx, conn, "SELECT COUNT(*) FROM orders GROUP BY user_id")
	assertReject(t, ctx, conn, "SELECT COUNT(*) AS c FROM orders GROUP BY c")
	assertReject(t, ctx, conn, "SELECT user_id, COUNT(*) FROM orders GROUP BY user_id WITH ROLLUP")
	assertReject(t, ctx, conn, "SELECT user_id, COUNT(*) FROM orders GROUP BY user_id ORDER BY COUNT(*)")
	assertReject(t, ctx, conn, "SELECT user_id, COUNT(*) FROM orders GROUP BY user_id HAVING COUNT(*) > 1 OR user_id = 1")
	assertReject(t, ctx, conn, "SELECT user_id, COUNT(*) FROM orders GROUP BY user_id HAVING COUNT(*) > '1'")
	assertReject(t, ctx, conn, "SELECT user_id, COUNT(*) FROM orders GROUP BY user_id HAVING SUM(qty) > 1")

	requireGroupRows(t, ctx, conn, "SELECT DISTINCT amount FROM orders",
		map[string][]string{"50": {}, "200": {}})
	requireGroupRows(t, ctx, conn, "SELECT DISTINCT user_id FROM orders",
		map[string][]string{"1": {}, "2": {}})
	requireOrderedColumn(t, ctx, conn, "SELECT DISTINCT amount FROM orders ORDER BY amount DESC",
		[]string{"200", "50"})
	requireOrderedColumn(t, ctx, conn, "SELECT DISTINCT amount FROM orders ORDER BY amount DESC LIMIT 1",
		[]string{"200"})
	requireRowCount(t, ctx, conn, "SELECT DISTINCT amount FROM orders LIMIT 1", 1)
	assertReject(t, ctx, conn, "SELECT DISTINCT name FROM profiles")
	assertReject(t, ctx, conn, "SELECT DISTINCT amount FROM orders ORDER BY user_id")

	// Unsupported shapes are explicit 1235, not silent wrong results.
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

func assertRoute(t *testing.T, ctx context.Context, conn *sql.Conn, query, kind, slice string) {
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
	require.Equal(t, "slave", gotRole)
}

func requireQueryHasRow(t *testing.T, ctx context.Context, conn *sql.Conn, query string) {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query)
	require.NoErrorf(t, err, "查询失败: %s", query)
	defer rows.Close()
	require.Truef(t, rows.Next(), "期望至少一行: %s", query)
	require.NoError(t, rows.Err())
}

func connScalarString(t *testing.T, ctx context.Context, conn *sql.Conn, query string) string {
	t.Helper()
	var s string
	require.NoErrorf(t, conn.QueryRowContext(ctx, query).Scan(&s), "查询失败: %s", query)
	return s
}

// requireShardRows asserts an order-insensitive (user_id, amount) result set
// and that no row is duplicated or missing.
func requireRowValues(t *testing.T, ctx context.Context, conn *sql.Conn, query string, want [][]string) {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query)
	require.NoErrorf(t, err, "查询失败: %s", query)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	var got [][]string
	for rows.Next() {
		cells := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		row := make([]string, len(cells))
		for i, c := range cells {
			row[i] = c.String
		}
		got = append(got, row)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, want, got, query)
}

func requireGroupRows(t *testing.T, ctx context.Context, conn *sql.Conn, query string, want map[string][]string) {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query)
	require.NoErrorf(t, err, "查询失败: %s", query)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	got := make(map[string][]string, len(want))
	n := 0
	for rows.Next() {
		cells := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		require.NoError(t, rows.Scan(ptrs...))
		key := cells[0].String
		vals := make([]string, 0, len(cols)-1)
		for _, c := range cells[1:] {
			vals = append(vals, c.String)
		}
		got[key] = vals
		n++
	}
	require.NoError(t, rows.Err())
	require.Equal(t, len(want), n, "组数不一致: %s", query)
	require.Equal(t, want, got, query)
}

func requireShardRows(t *testing.T, ctx context.Context, conn *sql.Conn, query string, want map[int]int) {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query)
	require.NoErrorf(t, err, "查询失败: %s", query)
	defer rows.Close()

	got := make(map[int]int, len(want))
	n := 0
	for rows.Next() {
		var userID, amount int
		require.NoError(t, rows.Scan(&userID, &amount))
		got[userID] = amount
		n++
	}
	require.NoError(t, rows.Err())
	require.Equal(t, len(want), n, "行数不一致: %s", query)
	require.Equal(t, want, got, query)
}

func requireOrderedColumn(t *testing.T, ctx context.Context, conn *sql.Conn, query string, want []string) {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query)
	require.NoErrorf(t, err, "查询失败: %s", query)
	defer rows.Close()
	var got []string
	for rows.Next() {
		var v string
		require.NoError(t, rows.Scan(&v))
		got = append(got, v)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, want, got, query)
}

func requireRowCount(t *testing.T, ctx context.Context, conn *sql.Conn, query string, want int) {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query)
	require.NoErrorf(t, err, "查询失败: %s", query)
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	require.NoError(t, rows.Err())
	require.Equalf(t, want, n, "行数不一致: %s", query)
}

func requireProfiles(t *testing.T, ctx context.Context, conn *sql.Conn, query string, want map[string]int) {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query)
	require.NoErrorf(t, err, "查询失败: %s", query)
	defer rows.Close()

	got := make(map[string]int, len(want))
	n := 0
	for rows.Next() {
		var name string
		var score int
		require.NoError(t, rows.Scan(&name, &score))
		got[name] = score
		n++
	}
	require.NoError(t, rows.Err())
	require.Equal(t, len(want), n, "行数不一致: %s", query)
	require.Equal(t, want, got, query)
}

func requireColumns(t *testing.T, ctx context.Context, conn *sql.Conn, query string, want []string) {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query)
	require.NoErrorf(t, err, "查询失败: %s", query)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	require.Equal(t, want, cols)
	require.True(t, rows.Next(), "期望至少一行: %s", query)
	require.NoError(t, rows.Err())
}

func requireRouteSlices(t *testing.T, ctx context.Context, conn *sql.Conn, query string, want []string) {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query)
	require.NoErrorf(t, err, "查询失败: %s", query)
	defer rows.Close()
	var slices []string
	for rows.Next() {
		var kind, slice, role, db, sqlText, reason string
		require.NoError(t, rows.Scan(&kind, &slice, &role, &db, &sqlText, &reason))
		require.Equal(t, "MultiShard", kind)
		slices = append(slices, slice)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, want, slices)
}
