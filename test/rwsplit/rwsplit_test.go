package rwsplit_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"

	"github.com/acorndb/shardweave/test/harness"
)

// TestRWSplitRealTopology drives real client requests through the proxy against
// a real master/replica GTID topology and checks that observed results are
// consistent with the direct-master oracle.
func TestRWSplitRealTopology(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过需要 Docker 的 rwsplit 拓扑测试")
	}
	harness.SkipIfNoDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	tp, err := harness.StartTopology(ctx, 2)
	require.NoError(t, err, "启动主从拓扑失败")
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer closeCancel()
		tp.Close(closeCtx)
	})
	t.Logf("topology: master=%s server_id=%d replicas=%v",
		tp.Master.Addr(), tp.Master.ServerID, tp.ReplicaServerIDs())

	proxy, err := harness.StartProxy(tp.ProxyConfig())
	require.NoError(t, err, "启动代理失败")
	t.Cleanup(proxy.Close)
	t.Logf("proxy listening on %s", proxy.Addr())

	_, err = tp.Master.DB.ExecContext(ctx, `CREATE TABLE probe (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
		v VARCHAR(64) NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	require.NoError(t, err)
	require.NoError(t, tp.Sync(ctx), "DDL 未同步到从库")

	t.Run("replicas_are_read_only", func(t *testing.T) {
		for _, r := range tp.Replicas {
			db, err := sql.Open("mysql", r.AppDSN())
			require.NoError(t, err)
			defer db.Close()
			_, err = db.ExecContext(ctx, "INSERT INTO probe(v) VALUES ('direct-replica')")
			var me *drivermysql.MySQLError
			require.ErrorAs(t, err, &me)
			require.Equal(t, uint16(1290), me.Number, "从库应拒绝应用账号写入")
			t.Logf("%s rejected write with errno=%d", r.Name, me.Number)
		}
	})

	t.Run("plain_select_routes_to_replica", func(t *testing.T) {
		conn := openProxyConn(t, ctx, proxy.DSN())
		var serverID int
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT @@server_id").Scan(&serverID))
		require.Contains(t, tp.ReplicaServerIDs(), serverID, "普通 SELECT 应落到从库")
		t.Logf("plain SELECT served by server_id=%d", serverID)
	})

	t.Run("write_then_read_through_proxy", func(t *testing.T) {
		conn := openProxyConn(t, ctx, proxy.DSN())
		res, err := conn.ExecContext(ctx, "INSERT INTO probe(v) VALUES ('via-proxy')")
		require.NoError(t, err)
		affected, err := res.RowsAffected()
		require.NoError(t, err)
		require.Equal(t, int64(1), affected)

		// Oracle: the write must have reached the master.
		var n int
		require.NoError(t, tp.Master.DB.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM probe WHERE v = 'via-proxy'").Scan(&n))
		require.Equal(t, 1, n, "代理写入未到达主库")

		require.NoError(t, tp.Sync(ctx), "写入未同步到从库")

		readConn := openProxyConn(t, ctx, proxy.DSN())
		require.NoError(t, readConn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM probe WHERE v = 'via-proxy'").Scan(&n))
		require.Equal(t, 1, n, "从库追平后代理读结果应与主库一致")
	})

	t.Run("transaction_pins_master", func(t *testing.T) {
		conn := openProxyConn(t, ctx, proxy.DSN())
		_, err := conn.ExecContext(ctx, "BEGIN")
		require.NoError(t, err)

		var serverID int
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT @@server_id").Scan(&serverID))
		require.Equal(t, tp.Master.ServerID, serverID, "事务内读应 pin 主库")

		_, err = conn.ExecContext(ctx, "INSERT INTO probe(v) VALUES ('tx')")
		require.NoError(t, err)

		var n int
		require.NoError(t, conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM probe WHERE v = 'tx'").Scan(&n))
		require.Equal(t, 1, n, "事务内应读到自己的写")

		_, err = conn.ExecContext(ctx, "COMMIT")
		require.NoError(t, err)
	})

	t.Run("prepared_statement_round_trip", func(t *testing.T) {
		conn := openProxyConn(t, ctx, proxy.DSN())
		stmt, err := conn.PrepareContext(ctx, "INSERT INTO probe(v) VALUES (?)")
		require.NoError(t, err)
		_, err = stmt.ExecContext(ctx, "prepared")
		require.NoError(t, err)
		require.NoError(t, stmt.Close())

		var n int
		require.NoError(t, conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM probe WHERE v = ?", "prepared").Scan(&n))
		require.Equal(t, 1, n, "prepared 写入后应能读回")
	})

	t.Run("user_variable_stays_on_one_backend", func(t *testing.T) {
		conn := openProxyConn(t, ctx, proxy.DSN())
		_, err := conn.ExecContext(ctx, "SET @probe = 42")
		require.NoError(t, err)

		var got int
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT @probe").Scan(&got))
		require.Equal(t, 42, got, "用户变量会话必须留在同一后端")
	})

	t.Run("mysql_error_passthrough", func(t *testing.T) {
		conn := openProxyConn(t, ctx, proxy.DSN())
		_, err := conn.ExecContext(ctx, "SELECT * FROM table_that_does_not_exist")
		var me *drivermysql.MySQLError
		require.ErrorAs(t, err, &me)
		require.Equal(t, uint16(1146), me.Number)
	})

	t.Run("randomized_select_differential", func(t *testing.T) {
		runRandomizedSelectDifferential(t, ctx, tp, proxy)
	})

	t.Run("sql_type_matrix", func(t *testing.T) {
		runSQLTypeMatrix(t, ctx, tp, proxy)
	})
}

func openProxyConn(t *testing.T, ctx context.Context, dsn string) *sql.Conn {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
		_ = db.Close()
	})
	return conn
}
