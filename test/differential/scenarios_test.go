package differential

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/acorndb/shardweave/pkg/backend"
	"github.com/acorndb/shardweave/pkg/execute"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/proxy"
	"github.com/acorndb/shardweave/pkg/route/source"
	"github.com/stretchr/testify/require"
	mysqltc "github.com/testcontainers/testcontainers-go/modules/mysql"
)

func TestQueryScenarios_NonEmpty(t *testing.T) {
	scs := QueryScenarios()
	require.NotEmpty(t, scs)
	for _, sc := range scs {
		require.NotEmpty(t, sc.Name)
		require.NotEmpty(t, sc.Steps)
	}
}

// TestPassthroughQueryScenarios runs assembled query scenes through the real
// proxy and compares each step's observation with a direct MySQL connection.
func TestPassthroughQueryScenarios(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过需要 Docker 的 query 场景差分")
	}
	skipIfNoDocker(t)

	ctx := context.Background()
	cfg, err := filepath.Abs(filepath.Join("testdata", "native.cnf"))
	require.NoError(t, err)

	c, err := mysqltc.Run(ctx, "mysql:8.0",
		mysqltc.WithDatabase(dbDirect),
		mysqltc.WithUsername(calibUser),
		mysqltc.WithPassword(calibPass),
		mysqltc.WithScripts(filepath.Join("testdata", "init.sql")),
		mysqltc.WithConfigFile(cfg),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	host, err := c.Host(ctx)
	require.NoError(t, err)
	mapped, err := c.MappedPort(ctx, "3306")
	require.NoError(t, err)
	mysqlAddr := net.JoinHostPort(host, mapped.Port())

	p := backend.NewPool(backend.PoolConfig{Capacity: 4, MaxCapacity: 16, AcquireTimeout: 5 * time.Second},
		func(ctx context.Context) (backend.Conn, error) {
			return backend.DialMySQL(ctx, mysqlAddr, calibUser, calibPass, dbProxied)
		})
	slice := &backend.Slice{Name: "default", Master: p}
	cluster, err := backend.NewCluster(slice)
	require.NoError(t, err)
	exec, err := execute.New(cluster, plan.Builder{Router: source.AllMaster{Slice: "default"}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = exec.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	serveCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_ = proxy.Serve(serveCtx, c, proxy.SessionConfig{
					Users: mapUsers{calibUser: calibPass},
					Exec:  exec,
				})
				_ = c.Close()
			}(conn)
		}
	}()

	directDSN := dsn(mysqlAddr, dbDirect)
	proxyDSN := dsn(ln.Addr().String(), dbProxied)

	for _, sc := range QueryScenarios() {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			direct, err := RunScript(ctx, directDSN, sc.Steps)
			require.NoError(t, err, "直连侧脚本执行失败")
			proxied, err := RunScript(ctx, proxyDSN, sc.Steps)
			require.NoError(t, err, "代理侧脚本执行失败")

			scrubSchemaRefs(direct)
			scrubSchemaRefs(proxied)

			diffs := Compare(sc.Steps, direct, proxied)
			for _, d := range diffs {
				t.Log(d)
			}
			require.Empty(t, diffs, "场景 %s 存在 %d 处差异", sc.Name, len(diffs))
		})
	}
}
