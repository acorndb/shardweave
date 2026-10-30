package differential

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	mysqltc "github.com/testcontainers/testcontainers-go/modules/mysql"
)

const (
	calibUser = "calib"
	calibPass = "calib-pass-123"
	dbDirect  = "calib_direct"
	dbProxied = "calib_proxied"
)

// TestNullProxyCalibration 是差分 harness 的校准测试:零号代理只搬运字节,
// 同一脚本经它与直连的观测必须完全一致。出现任何差异都意味着 harness 或
// nullproxy 自身有 bug——在它们修好之前,真代理的差分结果不可信。
func TestNullProxyCalibration(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过需要 Docker 的差分校准")
	}
	skipIfNoDocker(t)

	ctx := context.Background()
	c, err := mysqltc.Run(ctx, "mysql:8.0",
		mysqltc.WithDatabase(dbDirect),
		mysqltc.WithUsername(calibUser),
		mysqltc.WithPassword(calibPass),
		mysqltc.WithScripts(filepath.Join("testdata", "init.sql")), // 补建 proxied 侧库
	)
	require.NoError(t, err, "启动 MySQL 容器失败")
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "3306")
	require.NoError(t, err)
	mysqlAddr := net.JoinHostPort(host, port.Port())

	proxy, err := StartNullProxy("127.0.0.1:0", mysqlAddr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = proxy.Stop() })

	steps := CalibrationScript()

	direct, err := RunScript(ctx, dsn(mysqlAddr, dbDirect), steps)
	require.NoError(t, err, "直连侧脚本执行失败")
	proxied, err := RunScript(ctx, dsn(proxy.Addr().String(), dbProxied), steps)
	require.NoError(t, err, "代理侧脚本执行失败")

	// 两侧运行在不同 schema:错误/警告消息中的库名是部署差异而非行为差异,对比前归一化。
	scrubSchemaRefs(direct)
	scrubSchemaRefs(proxied)

	diffs := Compare(steps, direct, proxied)
	for _, d := range diffs {
		t.Log(d)
	}
	require.Empty(t, diffs, "零号代理校准失败: 存在 %d 处差异, harness 或 nullproxy 有 bug", len(diffs))
}

func scrubSchemaRefs(results []StepResult) {
	for i := range results {
		results[i].Err = scrubSQLError(results[i].Err)
		for j := range results[i].Execs {
			results[i].Execs[j].Err = scrubSQLError(results[i].Execs[j].Err)
		}
		for j := range results[i].Warnings {
			results[i].Warnings[j].Message = scrubSchema(results[i].Warnings[j].Message)
		}
	}
}

func scrubSQLError(e *SQLError) *SQLError {
	if e == nil {
		return nil
	}
	return &SQLError{Number: e.Number, Message: scrubSchema(e.Message)}
}

func scrubSchema(s string) string {
	s = strings.ReplaceAll(s, dbDirect, "<schema>")
	return strings.ReplaceAll(s, dbProxied, "<schema>")
}

func dsn(addr, db string) string {
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&multiStatements=true&loc=UTC",
		calibUser, calibPass, addr, db)
}

// skipIfNoDocker 探测 docker daemon(支持 DOCKER_HOST 与常见 socket 路径),
// 不可用时跳过测试。刻意不依赖 docker 客户端库:其模块路径近年多次变动
// (docker/docker 拆分为 moby/moby),harness 不应被它绑死。
func skipIfNoDocker(t *testing.T) {
	t.Helper()
	addr, network, ok := dockerDaemonAddr()
	if !ok {
		t.Skip("未找到 docker daemon(无 DOCKER_HOST 且无常见 socket), 跳过")
	}
	conn, err := net.DialTimeout(network, addr, 3*time.Second)
	if err != nil {
		t.Skipf("docker daemon 不可达(%s %s), 跳过: %v", network, addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET /_ping HTTP/1.0\r\n\r\n"); err != nil {
		t.Skipf("docker daemon ping 失败, 跳过: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Skipf("docker daemon 未就绪, 跳过: resp=%v err=%v", resp, err)
	}
}

func dockerDaemonAddr() (addr, network string, ok bool) {
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		if u, err := url.Parse(h); err == nil {
			switch u.Scheme {
			case "unix":
				return u.Path, "unix", true
			case "tcp", "http", "https":
				return u.Host, "tcp", true
			}
		}
	}
	for _, s := range []string{
		"/var/run/docker.sock",
		filepath.Join(os.Getenv("HOME"), ".docker/run/docker.sock"),
		filepath.Join(os.Getenv("HOME"), ".orbstack/run/docker.sock"),
	} {
		if st, err := os.Stat(s); err == nil && st.Mode()&os.ModeSocket != 0 {
			return s, "unix", true
		}
	}
	return "", "", false
}

// CalibrationScript 是差分语料 v0:覆盖 DDL/DML、聚合、表达式、用户变量、
// 主键冲突、prepared(二进制协议)、多语句、语法错误路径与 warning 路径。
// 语料只允许确定性语句(禁用 NOW() 等非确定函数),且不得引用库名
// (两侧运行在不同 schema,错误/警告消息中的库名由校准层归一化);
// 每个真实 bug 都应转为新条目。
func CalibrationScript() []Step {
	return []Step{
		{Name: "create-table", Exec: `CREATE TABLE items (
			id INT AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(64) NOT NULL,
			price DECIMAL(10,2),
			qty INT,
			created DATE
		)`},
		{Name: "insert-multi", Exec: `INSERT INTO items (name, price, qty, created) VALUES
			('椅子-chair', 12.50, 3, '2026-10-30'),
			('desk', 199.99, 1, '2026-01-02'),
			('lamp', NULL, 0, NULL)`},
		{Name: "select-all", Query: `SELECT id, name, price, qty, created FROM items ORDER BY id`},
		{Name: "select-agg", Query: `SELECT COUNT(*) AS cnt, SUM(qty) AS total_qty, AVG(price) AS avg_price,
			MIN(name) AS min_name, MAX(created) AS max_created FROM items`},
		{Name: "select-expr", Query: `SELECT id % 2 AS parity, CONCAT(name, '!') AS labeled,
			COALESCE(price, 0) + 1 AS priced FROM items ORDER BY id DESC`},
		{Name: "set-user-var", Exec: `SET @threshold := 10`},
		{Name: "read-user-var", Query: `SELECT @threshold, @undefined_var`},
		{Name: "update", Exec: `UPDATE items SET qty = qty + 5 WHERE id <= 2`},
		{Name: "dup-key-error", Exec: `INSERT INTO items (id, name) VALUES (1, 'dup')`},
		{Name: "limit-offset", Query: `SELECT id, name FROM items ORDER BY id LIMIT 2 OFFSET 1`},
		{Name: "self-join", Query: `SELECT a.id, b.name FROM items a JOIN items b ON b.id = a.id - 1 ORDER BY a.id`},
		{Name: "prepared-query", Prepared: &PreparedStep{
			Query: `SELECT id, name, qty FROM items WHERE id > ? ORDER BY id`,
			Runs: []PreparedRun{
				{Args: []any{1}, Query: true},
				{Args: []any{3}, Query: true},
				{Args: []any{nil}, Query: true},
			},
		}},
		{Name: "prepared-exec", Prepared: &PreparedStep{
			Query: `INSERT INTO items (name, price, qty) VALUES (?, ?, ?)`,
			Runs:  []PreparedRun{{Args: []any{"prepped", 7.25, 2}}},
		}},
		{Name: "multi-statement", Query: `SELECT COUNT(*) FROM items; SELECT name FROM items ORDER BY id DESC LIMIT 2`},
		{Name: "syntax-error", Query: `SELEKT 1`},
		{Name: "drop-if-exists-warning", Exec: `DROP TABLE IF EXISTS items_missing`},
		{Name: "drop-table", Exec: `DROP TABLE items`},
	}
}
