package rwsplit_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"

	"github.com/acorndb/shardweave/test/harness"
)

// indexMetadataQuery projects stable information_schema columns.
// SHOW INDEX includes Cardinality, which is an estimate and may differ between
// master and replica; it is not a proxy-forwarding property.
const indexMetadataQuery = `SELECT TABLE_NAME, NON_UNIQUE, KEY_NAME, SEQ_IN_INDEX,
	COLUMN_NAME, COLLATION, SUB_PART, INDEX_TYPE, NULLABLE, INDEX_COMMENT
	FROM information_schema.statistics
	WHERE table_schema = DATABASE() AND table_name = 'sqltype_users'
	ORDER BY KEY_NAME, SEQ_IN_INDEX`

// runSQLTypeMatrix exercises DDL, DML, SELECT shapes, SHOW, prepared
// statements, transactions, errors, charset and a >16MiB payload through the
// proxy, using the direct master as the oracle.
func runSQLTypeMatrix(t *testing.T, ctx context.Context, tp *harness.Topology, proxy *harness.Proxy) {
	t.Helper()

	master := tp.Master.DB
	pdb, err := sql.Open("mysql", proxy.DSN())
	require.NoError(t, err)
	pdb.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = pdb.Close() })

	setup := []string{
		`CREATE TABLE sqltype_users (
			id INT NOT NULL PRIMARY KEY,
			name VARCHAR(64) NOT NULL,
			age INT NOT NULL,
			dept VARCHAR(16) NOT NULL,
			created DATE NOT NULL,
			bio TEXT
		)`,
		`CREATE TABLE sqltype_orders (
			id INT NOT NULL PRIMARY KEY,
			user_id INT NOT NULL,
			amount DECIMAL(10,2) NOT NULL,
			note VARCHAR(64)
		)`,
		`CREATE TABLE sqltype_json (id INT NOT NULL PRIMARY KEY, doc JSON NOT NULL)`,
		`CREATE TABLE sqltype_null (id INT NOT NULL PRIMARY KEY, v INT NULL, s VARCHAR(8) NULL)`,
		`CREATE TABLE sqltype_big (id INT NOT NULL PRIMARY KEY, payload LONGTEXT NOT NULL)`,
		`CREATE TABLE sqltype_bin (id INT NOT NULL PRIMARY KEY, b VARBINARY(16) NOT NULL)`,
		`CREATE TABLE sqltype_auto (id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, v VARCHAR(32) NOT NULL)`,
		`CREATE TABLE sqltype_parent (id INT NOT NULL PRIMARY KEY)`,
		`CREATE TABLE sqltype_child (
			id INT NOT NULL PRIMARY KEY,
			pid INT NOT NULL,
			KEY idx_pid (pid),
			CONSTRAINT fk_child_parent FOREIGN KEY (pid) REFERENCES sqltype_parent(id)
		)`,
		`INSERT INTO sqltype_users VALUES
			(1,'alice',30,'eng','2024-01-01','bio-a'),
			(2,'bob',25,'eng','2024-02-02',NULL),
			(3,'carol',41,'ops','2024-03-03','bio-c'),
			(4,'dave',19,'ops','2024-04-04',NULL),
			(5,'eve',35,'sales','2024-05-05','bio-e'),
			(6,'frank',22,'eng','2024-06-06','bio-f')`,
		`INSERT INTO sqltype_orders VALUES
			(1,1,10.50,'o1'),
			(2,1,20.00,NULL),
			(3,3,5.25,'o3'),
			(4,5,99.99,'o5'),
			(5,6,1.00,'o6')`,
		`INSERT INTO sqltype_json VALUES
			(1,'{"k":"v","n":1}'),
			(2,'{"k":"w","n":2}')`,
		`INSERT INTO sqltype_null VALUES (1,10,'x'),(2,NULL,NULL),(3,30,'z')`,
		`INSERT INTO sqltype_parent VALUES (1),(2)`,
		`CREATE PROCEDURE sqltype_two_results() BEGIN SELECT 1 AS a; SELECT 2 AS b; END`,
	}
	for _, stmt := range setup {
		mustExec(t, ctx, pdb, stmt)
	}
	require.NoError(t, tp.Sync(ctx), "初始化数据未同步到从库")

	t.Run("select_shapes", func(t *testing.T) {
		queries := []struct {
			name  string
			query string
		}{
			{"join", `SELECT u.name, o.amount FROM sqltype_users u
				JOIN sqltype_orders o ON o.user_id = u.id ORDER BY u.id, o.id`},
			{"group_by_having", `SELECT dept, COUNT(*), SUM(age), MIN(age), MAX(age)
				FROM sqltype_users GROUP BY dept HAVING COUNT(*) >= 1 ORDER BY dept`},
			{"union_all", `SELECT id FROM sqltype_users WHERE id <= 2
				UNION ALL SELECT id FROM sqltype_orders WHERE id <= 2 ORDER BY id`},
			{"cte", `WITH young AS (SELECT id, age FROM sqltype_users WHERE age < 30)
				SELECT COUNT(*), SUM(age) FROM young`},
			{"window", `SELECT id, ROW_NUMBER() OVER (PARTITION BY dept ORDER BY age) AS rn
				FROM sqltype_users ORDER BY id`},
			{"exists", `SELECT u.id FROM sqltype_users u
				WHERE EXISTS (SELECT 1 FROM sqltype_orders o WHERE o.user_id = u.id) ORDER BY u.id`},
			{"case_cast_coalesce", `SELECT id,
				CASE WHEN age >= 30 THEN 'senior' ELSE 'junior' END AS band,
				CAST(age AS CHAR) AS age_s, COALESCE(bio, '-') AS bio
				FROM sqltype_users ORDER BY id`},
			{"json", `SELECT id, JSON_EXTRACT(doc, '$.k') AS k,
				JSON_UNQUOTE(JSON_EXTRACT(doc, '$.n')) AS n FROM sqltype_json ORDER BY id`},
			{"string_date", `SELECT id, CONCAT(name, '-', dept) AS tag,
				DATE_FORMAT(created, '%Y/%m') AS ym, LEFT(name, 2) AS prefix
				FROM sqltype_users ORDER BY id`},
			{"distinct_limit_offset", `SELECT DISTINCT dept FROM sqltype_users
				ORDER BY dept LIMIT 2 OFFSET 1`},
			{"aggregates_with_null", `SELECT COUNT(*), COUNT(v), SUM(v), AVG(v) FROM sqltype_null`},
			{"in_subquery", `SELECT id FROM sqltype_users
				WHERE id IN (SELECT user_id FROM sqltype_orders WHERE amount > 10) ORDER BY id`},
			{"recursive_cte", `WITH RECURSIVE seq AS (
				SELECT 1 AS n UNION ALL SELECT n + 1 FROM seq WHERE n < 5)
				SELECT n FROM seq ORDER BY n`},
			{"group_concat", `SELECT dept, GROUP_CONCAT(name ORDER BY id SEPARATOR ',')
				FROM sqltype_users GROUP BY dept ORDER BY dept`},
		}
		for _, tc := range queries {
			t.Run(tc.name, func(t *testing.T) {
				requireSameQuery(t, ctx, master, pdb, tc.query)
			})
		}
	})

	t.Run("show_statements", func(t *testing.T) {
		for _, q := range []string{
			"SHOW TABLES LIKE 'sqltype%'",
			"SHOW COLUMNS FROM sqltype_users",
			indexMetadataQuery,
			"SHOW CREATE TABLE sqltype_orders",
			"SHOW VARIABLES LIKE 'sql_mode'",
		} {
			t.Run(q, func(t *testing.T) {
				requireSameQuery(t, ctx, master, pdb, q)
			})
		}
	})

	t.Run("prepared_statements", func(t *testing.T) {
		requireSameQuery(t, ctx, master, pdb,
			"SELECT id, name, age, bio FROM sqltype_users WHERE age > ? ORDER BY id", 25)
		requireSameQuery(t, ctx, master, pdb,
			"SELECT id, v, s FROM sqltype_null WHERE id = ?", 2)
		requireSameQuery(t, ctx, master, pdb,
			"SELECT id, amount FROM sqltype_orders WHERE amount BETWEEN ? AND ? ORDER BY id", 5.0, 21.0)

		mustExec(t, ctx, pdb,
			"INSERT INTO sqltype_orders (id, user_id, amount, note) VALUES (?, ?, ?, ?)",
			90, 2, 3.30, "prepared")
		require.Equal(t, "prepared", masterScalar(t, ctx, master,
			"SELECT note FROM sqltype_orders WHERE id = 90"))

		mustExec(t, ctx, pdb, "UPDATE sqltype_orders SET note = ? WHERE id = ?", "prepared2", 90)
		require.Equal(t, "prepared2", masterScalar(t, ctx, master,
			"SELECT note FROM sqltype_orders WHERE id = 90"))

		mustExec(t, ctx, pdb, "DELETE FROM sqltype_orders WHERE id = ?", 90)
		require.Equal(t, "0", masterScalar(t, ctx, master,
			"SELECT COUNT(*) FROM sqltype_orders WHERE id = 90"))
	})

	t.Run("session_functions", func(t *testing.T) {
		conn := openProxyConn(t, ctx, proxy.DSN())

		_, err := conn.ExecContext(ctx, "INSERT INTO sqltype_auto (v) VALUES ('text-last-id')")
		require.NoError(t, err)
		id := dbScalarInt64(t, ctx, master, "SELECT id FROM sqltype_auto WHERE v = 'text-last-id'")
		require.Equal(t, id, connScalarInt64(t, ctx, conn, "SELECT LAST_INSERT_ID()"))

		rows, err := conn.QueryContext(ctx, "SELECT LAST_INSERT_ID() AS last_id")
		require.NoError(t, err)
		cols, err := rows.Columns()
		require.NoError(t, err)
		require.Equal(t, []string{"last_id"}, cols)
		require.True(t, rows.Next())
		var aliasID int64
		require.NoError(t, rows.Scan(&aliasID))
		require.NoError(t, rows.Close())
		require.Equal(t, id, aliasID)

		res, err := conn.ExecContext(ctx,
			"UPDATE sqltype_auto SET v = 'changed' WHERE id = ?", id)
		require.NoError(t, err)
		affected, err := res.RowsAffected()
		require.NoError(t, err)
		require.Equal(t, int64(1), affected)
		require.Equal(t, affected, connScalarInt64(t, ctx, conn, "SELECT ROW_COUNT()"))
		require.Equal(t, int64(-1), connScalarInt64(t, ctx, conn, "SELECT ROW_COUNT()"),
			"the first ROW_COUNT() query is itself a SELECT")

		require.Equal(t, id, connScalarInt64(t, ctx, conn, "SELECT LAST_INSERT_ID()"),
			"LAST_INSERT_ID persists after reads")

		stmt, err := conn.PrepareContext(ctx, "INSERT INTO sqltype_auto (v) VALUES (?)")
		require.NoError(t, err)
		_, err = stmt.ExecContext(ctx, "prepared-last-id")
		require.NoError(t, err)
		require.NoError(t, stmt.Close())
		preparedID := dbScalarInt64(t, ctx, master, "SELECT id FROM sqltype_auto WHERE v = 'prepared-last-id'")
		require.Equal(t, preparedID, connScalarInt64(t, ctx, conn, "SELECT LAST_INSERT_ID()"))

		_, err = pdb.PrepareContext(ctx, "SELECT LAST_INSERT_ID()")
		var me *drivermysql.MySQLError
		require.ErrorAs(t, err, &me)
		require.Equal(t, uint16(1235), me.Number)

		for _, q := range []string{
			"SELECT FOUND_ROWS()",
			"SELECT SQL_CALC_FOUND_ROWS id FROM sqltype_users LIMIT 1",
		} {
			obs := observeExec(ctx, pdb, q)
			require.Error(t, obs.err, q)
			require.Equal(t, uint16(1235), obs.errNo, q)
		}
	})

	t.Run("connection_scoped", func(t *testing.T) {
		conn := openProxyConn(t, ctx, proxy.DSN())

		mustExecConn(t, ctx, conn, "CREATE TEMPORARY TABLE tmp_state (id INT NOT NULL PRIMARY KEY, v INT NOT NULL)")
		mustExecConn(t, ctx, conn, "INSERT INTO tmp_state VALUES (1, 10), (2, 20)")
		var n int
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM tmp_state").Scan(&n))
		require.Equal(t, 2, n, "临时表必须留在同一后端")
		mustExecConn(t, ctx, conn, "DROP TEMPORARY TABLE tmp_state")

		mustExecConn(t, ctx, conn, "SELECT 42 INTO @sv")
		var got int
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT @sv").Scan(&got))
		require.Equal(t, 42, got, "SELECT INTO 用户变量必须留在同一后端")

		var acquired sql.NullInt64
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT GET_LOCK('shardweave-probe', 1)").Scan(&acquired))
		require.Equal(t, int64(1), acquired.Int64)
		var released sql.NullInt64
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK('shardweave-probe')").Scan(&released))
		require.Equal(t, int64(1), released.Int64, "命名锁必须在同一后端释放")

		mustExecConn(t, ctx, conn, "LOCK TABLES sqltype_users READ")
		var cnt int
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqltype_users").Scan(&cnt))
		require.Greater(t, cnt, 0)
		mustExecConn(t, ctx, conn, "UNLOCK TABLES")
	})

	t.Run("multi_statement", func(t *testing.T) {
		masterMulti := openDB(t, tp.Master.AppDSN()+"&multiStatements=true")
		proxyMulti := openDB(t, proxy.DSN()+"&multiStatements=true")
		direct := multiResultSets(t, ctx, masterMulti, "SELECT 1 AS a; SELECT 2 AS b")
		proxied := multiResultSets(t, ctx, proxyMulti, "SELECT 1 AS a; SELECT 2 AS b")
		require.Equal(t, direct, proxied, "多语句结果集不一致")
		require.Equal(t, [][]string{{"1"}, {"2"}}, direct)
	})

	t.Run("multi_result_sets", func(t *testing.T) {
		direct := callResultSets(t, ctx, master)
		proxied := callResultSets(t, ctx, pdb)
		require.Equal(t, direct, proxied, "CALL 多结果集不一致")
		require.Equal(t, [][]string{{"1"}, {"2"}}, direct)
	})

	t.Run("binary_blob", func(t *testing.T) {
		blob := []byte{0x00, 0x01, 0x02, 0xff}
		mustExec(t, ctx, pdb, "INSERT INTO sqltype_bin (id, b) VALUES (?, ?)", 1, blob)
		require.NoError(t, tp.Sync(ctx))
		require.Equal(t, "000102FF", masterScalar(t, ctx, master,
			"SELECT HEX(b) FROM sqltype_bin WHERE id = 1"))
		requireSameQuery(t, ctx, master, pdb, "SELECT HEX(b), LENGTH(b) FROM sqltype_bin WHERE id = 1")
	})

	t.Run("transactions", func(t *testing.T) {
		conn := openProxyConn(t, ctx, proxy.DSN())

		_, err := conn.ExecContext(ctx, "BEGIN")
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx,
			"INSERT INTO sqltype_users (id,name,age,dept,created) VALUES (100,'rollback',1,'x','2024-01-01')")
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, "ROLLBACK")
		require.NoError(t, err)
		require.Equal(t, "0", masterScalar(t, ctx, master,
			"SELECT COUNT(*) FROM sqltype_users WHERE id = 100"))

		_, err = conn.ExecContext(ctx, "BEGIN")
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx,
			"INSERT INTO sqltype_users (id,name,age,dept,created) VALUES (101,'commit',1,'x','2024-01-01')")
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, "COMMIT")
		require.NoError(t, err)
		require.Equal(t, "1", masterScalar(t, ctx, master,
			"SELECT COUNT(*) FROM sqltype_users WHERE id = 101"))

		_, err = conn.ExecContext(ctx, "BEGIN")
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx,
			"INSERT INTO sqltype_users (id,name,age,dept,created) VALUES (102,'savepoint-a',1,'x','2024-01-01')")
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, "SAVEPOINT s1")
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx,
			"INSERT INTO sqltype_users (id,name,age,dept,created) VALUES (103,'savepoint-b',1,'x','2024-01-01')")
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, "ROLLBACK TO SAVEPOINT s1")
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, "COMMIT")
		require.NoError(t, err)
		require.Equal(t, "1", masterScalar(t, ctx, master,
			"SELECT COUNT(*) FROM sqltype_users WHERE id = 102"))
		require.Equal(t, "0", masterScalar(t, ctx, master,
			"SELECT COUNT(*) FROM sqltype_users WHERE id = 103"))

		_, err = conn.ExecContext(ctx, "SET autocommit = 0")
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx,
			"INSERT INTO sqltype_users (id,name,age,dept,created) VALUES (104,'autocommit',1,'x','2024-01-01')")
		require.NoError(t, err)
		var n int
		require.NoError(t, conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM sqltype_users WHERE id = 104").Scan(&n))
		require.Equal(t, 1, n, "autocommit=0 时应读到未提交的写")
		_, err = conn.ExecContext(ctx, "COMMIT")
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, "SET autocommit = 1")
		require.NoError(t, err)
		require.Equal(t, "1", masterScalar(t, ctx, master,
			"SELECT COUNT(*) FROM sqltype_users WHERE id = 104"))

		_, err = conn.ExecContext(ctx, "BEGIN")
		require.NoError(t, err)
		var serverID, id int
		require.NoError(t, conn.QueryRowContext(ctx,
			"SELECT @@server_id, id FROM sqltype_users WHERE id = 1 FOR UPDATE").Scan(&serverID, &id))
		require.Equal(t, tp.Master.ServerID, serverID, "锁定读必须落主库")
		_, err = conn.ExecContext(ctx, "COMMIT")
		require.NoError(t, err)

		_, err = conn.ExecContext(ctx, "START TRANSACTION")
		require.NoError(t, err)
		var shareServerID int
		require.NoError(t, conn.QueryRowContext(ctx,
			"SELECT @@server_id FROM sqltype_users WHERE id = 1 FOR SHARE").Scan(&shareServerID))
		require.Equal(t, tp.Master.ServerID, shareServerID, "FOR SHARE 必须落主库")
		_, err = conn.ExecContext(ctx, "COMMIT")
		require.NoError(t, err)
	})

	t.Run("errors", func(t *testing.T) {
		cases := []struct {
			name  string
			query string
			args  []any
			errno uint16
		}{
			{
				"duplicate_key",
				"INSERT INTO sqltype_users (id,name,age,dept,created) VALUES (1,'dup',1,'x','2024-01-01')", nil, 1062,
			},
			{"unknown_table", "SELECT * FROM sqltype_missing", nil, 1146},
			{"syntax", "SELECT FROM", nil, 1064},
			{
				"too_long",
				"INSERT INTO sqltype_null (id,s) VALUES (900, ?)",
				[]any{strings.Repeat("x", 20)},
				1406,
			},
			{
				"foreign_key",
				"INSERT INTO sqltype_child (id,pid) VALUES (900, 999)", nil, 1452,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				direct := observeExec(ctx, master, tc.query, tc.args...)
				proxied := observeExec(ctx, pdb, tc.query, tc.args...)
				require.NotNil(t, direct.err, "直连应报错")
				require.Equal(t, tc.errno, direct.errNo)
				require.True(t, direct.equal(proxied),
					"错误不一致:\nSQL: %s\n直连: %s\n代理: %s", tc.query, direct, proxied)
			})
		}
	})

	t.Run("unsupported_statements", func(t *testing.T) {
		for _, query := range []string{
			"SHOW WARNINGS",
			"SHOW ERRORS",
			"SHOW COUNT(*) WARNINGS",
			"SHOW MASTER STATUS",
			"SHOW BINARY LOGS",
			"SHOW REPLICA STATUS",
			"SET sql_mode = 'ANSI_QUOTES'",
			"SET @@SESSION.sql_mode = ''",
		} {
			obs := observeExec(ctx, pdb, query)
			require.Error(t, obs.err, query)
			require.Equal(t, uint16(1235), obs.errNo, query)
		}
		// Rejected statements must not break the session.
		requireSameQuery(t, ctx, master, pdb, "SELECT 1")
	})

	t.Run("charset", func(t *testing.T) {
		conn := openProxyConn(t, ctx, proxy.DSN())
		_, err := conn.ExecContext(ctx, "SET NAMES utf8mb4")
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx,
			"INSERT INTO sqltype_users (id,name,age,dept,created,bio) VALUES (200,'测试😀',20,'i18n','2024-01-01','中文')")
		require.NoError(t, err)
		require.NoError(t, tp.Sync(ctx))
		requireSameQuery(t, ctx, master, pdb,
			"SELECT name, bio, HEX(name) FROM sqltype_users WHERE id = 200")
	})

	t.Run("large_payload", func(t *testing.T) {
		limit := masterScalar(t, ctx, master, "SELECT @@GLOBAL.max_allowed_packet")
		require.NotEqual(t, "0", limit)

		payload := strings.Repeat("x", 17<<20) // 16MiB+1: forces multi-frame encoding
		mustExec(t, ctx, pdb, "INSERT INTO sqltype_big (id, payload) VALUES (?, ?)", 1, payload)
		require.NoError(t, tp.Sync(ctx))
		require.Equal(t, "17825792", masterScalar(t, ctx, master,
			"SELECT LENGTH(payload) FROM sqltype_big WHERE id = 1"))
		requireSameQuery(t, ctx, master, pdb,
			"SELECT LENGTH(payload) FROM sqltype_big WHERE id = 1")
	})

	t.Run("ddl_dml_lifecycle", func(t *testing.T) {
		mustExec(t, ctx, pdb, "ALTER TABLE sqltype_users ADD COLUMN nickname VARCHAR(32) NULL")
		require.NoError(t, tp.Sync(ctx))
		requireSameQuery(t, ctx, master, pdb, "SHOW COLUMNS FROM sqltype_users")

		mustExec(t, ctx, pdb, "CREATE INDEX idx_sqltype_users_dept ON sqltype_users (dept)")
		require.NoError(t, tp.Sync(ctx))
		requireSameQuery(t, ctx, master, pdb, indexMetadataQuery)
		requireSameQuery(t, ctx, master, pdb, "CHECK TABLE sqltype_users")

		mustExec(t, ctx, pdb, "ALTER TABLE sqltype_users MODIFY COLUMN nickname VARCHAR(64) NULL")
		require.NoError(t, tp.Sync(ctx))
		requireSameQuery(t, ctx, master, pdb, "SHOW COLUMNS FROM sqltype_users")

		mustExec(t, ctx, pdb, "ALTER TABLE sqltype_users DROP COLUMN nickname")
		require.NoError(t, tp.Sync(ctx))
		requireSameQuery(t, ctx, master, pdb, "SHOW COLUMNS FROM sqltype_users")

		mustExec(t, ctx, pdb,
			"CREATE VIEW sqltype_view AS SELECT id, name, age FROM sqltype_users WHERE age >= 30")
		require.NoError(t, tp.Sync(ctx))
		requireSameQuery(t, ctx, master, pdb, "SELECT * FROM sqltype_view ORDER BY id")

		mustExec(t, ctx, pdb, `INSERT INTO sqltype_users
			SET id = 300, name = 'insert-set', age = 50, dept = 'ops', created = '2024-01-01'`)
		require.Equal(t, "1", masterScalar(t, ctx, master,
			"SELECT COUNT(*) FROM sqltype_users WHERE id = 300"))

		mustExec(t, ctx, pdb, `INSERT INTO sqltype_users (id,name,age,dept,created,bio)
			VALUES (1,'alice-upd',31,'eng','2024-01-01','bio-a')
			ON DUPLICATE KEY UPDATE name = VALUES(name), age = VALUES(age)`)
		require.Equal(t, "alice-upd", masterScalar(t, ctx, master,
			"SELECT name FROM sqltype_users WHERE id = 1"))

		mustExec(t, ctx, pdb, `REPLACE INTO sqltype_users (id,name,age,dept,created)
			VALUES (6,'frank-2',23,'eng','2024-06-06')`)
		require.Equal(t, "frank-2", masterScalar(t, ctx, master,
			"SELECT name FROM sqltype_users WHERE id = 6"))

		mustExec(t, ctx, pdb, "UPDATE sqltype_users SET age = 40 WHERE id = 2")
		require.Equal(t, "40", masterScalar(t, ctx, master,
			"SELECT age FROM sqltype_users WHERE id = 2"))

		mustExec(t, ctx, pdb, "DELETE FROM sqltype_orders WHERE id = 2")
		require.Equal(t, "4", masterScalar(t, ctx, master,
			"SELECT COUNT(*) FROM sqltype_orders"))

		mustExec(t, ctx, pdb, `UPDATE sqltype_users u
			JOIN sqltype_orders o ON o.user_id = u.id
			SET u.age = u.age + 1 WHERE o.id = 3`)
		require.Equal(t, "42", masterScalar(t, ctx, master,
			"SELECT age FROM sqltype_users WHERE id = 3"))

		mustExec(t, ctx, pdb, `DELETE u FROM sqltype_users u
			JOIN sqltype_orders o ON o.user_id = u.id WHERE o.id = 3`)
		require.Equal(t, "0", masterScalar(t, ctx, master,
			"SELECT COUNT(*) FROM sqltype_users WHERE id = 3"))

		mustExec(t, ctx, pdb, `INSERT INTO sqltype_orders (id,user_id,amount,note)
			SELECT id + 200, id, 1.00, 'copy' FROM sqltype_users WHERE id <= 2`)
		require.Equal(t, "2", masterScalar(t, ctx, master,
			"SELECT COUNT(*) FROM sqltype_orders WHERE note = 'copy'"))

		mustExec(t, ctx, pdb, `CREATE TABLE sqltype_copy AS
			SELECT id, name, age FROM sqltype_users WHERE age >= 30`)
		require.NoError(t, tp.Sync(ctx))
		requireSameQuery(t, ctx, master, pdb, "SELECT * FROM sqltype_copy ORDER BY id")

		mustExec(t, ctx, pdb, "RENAME TABLE sqltype_copy TO sqltype_renamed")
		require.NoError(t, tp.Sync(ctx))
		requireSameQuery(t, ctx, master, pdb, "SELECT * FROM sqltype_renamed ORDER BY id")

		mustExec(t, ctx, pdb, "TRUNCATE TABLE sqltype_json")
		require.Equal(t, "0", masterScalar(t, ctx, master,
			"SELECT COUNT(*) FROM sqltype_json"))
		require.NoError(t, tp.Sync(ctx))
		requireSameQuery(t, ctx, master, pdb, "SELECT COUNT(*) FROM sqltype_json")

		mustExec(t, ctx, pdb, "DROP TABLE sqltype_json")
		require.Equal(t, "0", masterScalar(t, ctx, master,
			"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'sqltype_json'"))
		require.NoError(t, tp.Sync(ctx))
		requireSameQuery(t, ctx, master, pdb,
			"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'sqltype_json'")

		mustExec(t, ctx, pdb, "DROP VIEW sqltype_view")
		mustExec(t, ctx, pdb, "DROP TABLE sqltype_renamed")
		require.NoError(t, tp.Sync(ctx))
		requireSameQuery(t, ctx, master, pdb,
			"SELECT COUNT(*) FROM information_schema.views WHERE table_schema = DATABASE() AND table_name = 'sqltype_view'")
		requireSameQuery(t, ctx, master, pdb,
			"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'sqltype_renamed'")
	})
}

func mustExec(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) {
	t.Helper()
	_, err := db.ExecContext(ctx, query, args...)
	require.NoErrorf(t, err, "执行失败: %s", query)
}

func mustExecConn(t *testing.T, ctx context.Context, c *sql.Conn, query string, args ...any) {
	t.Helper()
	_, err := c.ExecContext(ctx, query, args...)
	require.NoErrorf(t, err, "执行失败: %s", query)
}

func openDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// callResultSets consumes every result set returned by CALL.
func callResultSets(t *testing.T, ctx context.Context, db *sql.DB) [][]string {
	t.Helper()
	return multiResultSets(t, ctx, db, "CALL sqltype_two_results()")
}

func multiResultSets(t *testing.T, ctx context.Context, db *sql.DB, query string) [][]string {
	t.Helper()
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	rows, err := db.QueryContext(qctx, query)
	require.NoError(t, err)
	defer rows.Close()

	var sets [][]string
	for {
		var vals []string
		for rows.Next() {
			var v sql.NullString
			require.NoError(t, rows.Scan(&v))
			if v.Valid {
				vals = append(vals, v.String)
			} else {
				vals = append(vals, "NULL")
			}
		}
		require.NoError(t, rows.Err())
		sets = append(sets, vals)
		if !rows.NextResultSet() {
			break
		}
	}
	return sets
}

func requireSameQuery(t *testing.T, ctx context.Context, direct, proxied *sql.DB, query string, args ...any) {
	t.Helper()
	d := observeQueryArgs(ctx, direct, query, args...)
	p := observeQueryArgs(ctx, proxied, query, args...)
	if !d.equal(p) {
		t.Fatalf("查询结果不一致:\nSQL: %s\n直连: %s\n代理: %s", query, d, p)
	}
}

func masterScalar(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) string {
	t.Helper()
	obs := observeQueryArgs(ctx, db, query, args...)
	require.NoErrorf(t, obs.err, "查询失败: %s", query)
	require.Len(t, obs.rows, 1, "期望单行结果: %s", query)
	require.Len(t, obs.rows[0], 1, "期望单列结果: %s", query)
	return obs.rows[0][0]
}

func dbScalarInt64(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoErrorf(t, db.QueryRowContext(ctx, query, args...).Scan(&n), "查询失败: %s", query)
	return n
}

func connScalarInt64(t *testing.T, ctx context.Context, conn *sql.Conn, query string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoErrorf(t, conn.QueryRowContext(ctx, query, args...).Scan(&n), "查询失败: %s", query)
	return n
}

type execObs struct {
	affected int64
	err      error
	errNo    uint16
	errMsg   string
}

func (e execObs) String() string {
	if e.err != nil {
		return fmt.Sprintf("errno=%d msg=%q", e.errNo, e.errMsg)
	}
	return fmt.Sprintf("affected=%d", e.affected)
}

func (e execObs) equal(other execObs) bool {
	if e.err != nil || other.err != nil {
		return e.err != nil && other.err != nil &&
			e.errNo == other.errNo && e.errMsg == other.errMsg
	}
	return e.affected == other.affected
}

func observeExec(ctx context.Context, db *sql.DB, query string, args ...any) execObs {
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return execObs{err: err, errNo: mysqlErrNo(err), errMsg: err.Error()}
	}
	n, err := res.RowsAffected()
	if err != nil {
		return execObs{err: err, errMsg: err.Error()}
	}
	return execObs{affected: n}
}
