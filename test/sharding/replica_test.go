package sharding_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/acorndb/shardweave/test/harness"
)

// TestShardedReplicas validates the implemented query path against a realistic
// topology: 2 slices, each with a master and one GTID replica. Writes go to the
// master, plain SELECTs may be served by the slice replica, and tp.Sync makes
// read-after-write deterministic.
func TestShardedReplicas(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过需要 Docker 的分片复制测试")
	}
	harness.SkipIfNoDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	tp, err := harness.StartShardedTopologyWithReplicas(ctx, 2, 1)
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
	require.NoError(t, tp.Sync(ctx), "DDL must reach every slice replica")

	for _, replicas := range tp.Replicas {
		for _, r := range replicas {
			var readOnly int
			require.NoError(t, r.DB.QueryRowContext(ctx, "SELECT @@global.read_only").Scan(&readOnly))
			require.Equal(t, 1, readOnly, "replica %s must be read_only", r.Name)
		}
	}

	proxy, err := harness.StartProxy(tp.ProxyConfig())
	require.NoError(t, err)
	t.Cleanup(proxy.Close)

	conn := openConn(t, ctx, proxy.DSN())

	// Writes are routed to each slice master.
	_, err = conn.ExecContext(ctx, "INSERT INTO orders (user_id, amount) VALUES (2, 100)")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "INSERT INTO orders (user_id, amount) VALUES (1, 50)")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "INSERT INTO profiles (name, score) VALUES ('alice', 1)")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "INSERT INTO profiles (name, score) VALUES ('bob', 2)")
	require.NoError(t, err)
	require.NoError(t, tp.Sync(ctx))

	// Plain reads use the slice replica; the default slice is s0.
	var serverID int
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT @@server_id").Scan(&serverID))
	require.Equal(t, tp.Replicas[0][0].ServerID, serverID, "plain SELECT should be served by s0 replica")
	requireQueryHasRow(t, ctx, conn, "SHOW COLUMNS FROM orders")

	// Scatter + concat from replicas.
	requireShardRows(t, ctx, conn, "SELECT user_id, amount FROM orders",
		map[int]int{1: 50, 2: 100})
	requireProfiles(t, ctx, conn, "SELECT name, score FROM profiles",
		map[string]int{"alice": 1, "bob": 2})
	requireOrderedColumn(t, ctx, conn, "SELECT user_id FROM orders ORDER BY user_id", []string{"1", "2"})
	requireOrderedColumn(t, ctx, conn, "SELECT user_id FROM orders ORDER BY user_id DESC", []string{"2", "1"})
	requireOrderedColumn(t, ctx, conn, "SELECT user_id FROM orders ORDER BY user_id LIMIT 1 OFFSET 1", []string{"2"})
	requireOrderedColumn(t, ctx, conn, "SELECT amount FROM orders ORDER BY user_id", []string{"50", "100"})
	requireOrderedColumn(t, ctx, conn, "SELECT amount FROM orders ORDER BY amount + 1", []string{"50", "100"})

	var count, sum, min, max int
	require.NoError(t, conn.QueryRowContext(ctx,
		"SELECT COUNT(*), SUM(amount), MIN(amount), MAX(amount) FROM orders").
		Scan(&count, &sum, &min, &max))
	require.Equal(t, []int{2, 150, 50, 100}, []int{count, sum, min, max})
	require.Equal(t, 2, connScalar(t, ctx, conn, "SELECT COUNT(*) AS c FROM profiles"))
	assertRoute(t, ctx, conn, "EXPLAIN ROUTE SELECT amount FROM orders WHERE user_id = 2",
		"SingleShard", "s0")

	// Prepared SELECT stays on the master and does not need replication sync.
	prep, err := conn.PrepareContext(ctx, "SELECT amount FROM orders WHERE user_id = 2 AND amount > ?")
	require.NoError(t, err)
	var prepared int
	require.NoError(t, prep.QueryRowContext(ctx, 0).Scan(&prepared))
	require.Equal(t, 100, prepared)
	require.NoError(t, prep.Close())

	// Write after read; sync then read from the replica again.
	_, err = conn.ExecContext(ctx, "UPDATE orders SET amount = 200 WHERE user_id = 2")
	require.NoError(t, err)
	require.NoError(t, tp.Sync(ctx))
	require.Equal(t, 200, connScalar(t, ctx, conn, "SELECT amount FROM orders WHERE user_id = 2"))
	requireShardRows(t, ctx, conn, "SELECT user_id, amount FROM orders",
		map[int]int{1: 50, 2: 200})
	var avg float64
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT AVG(amount) FROM orders").Scan(&avg))
	require.Equal(t, 125.0, avg)

	// Unsupported shapes stay explicit errors on a real topology.
	assertReject(t, ctx, conn, "SELECT AVG(DISTINCT amount) FROM orders")
	assertReject(t, ctx, conn, "SELECT COUNT(DISTINCT user_id) FROM orders")
	requireGroupRows(t, ctx, conn,
		"SELECT user_id, COUNT(*) FROM orders GROUP BY user_id",
		map[string][]string{"1": {"1"}, "2": {"1"}})
	assertReject(t, ctx, conn, "SELECT * FROM orders WHERE user_id IN (SELECT user_id FROM orders)")
	requireRowCount(t, ctx, conn, "SELECT * FROM orders LIMIT 1, 1", 1)
}
