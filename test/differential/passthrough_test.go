package differential

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/acorndb/prism/pkg/backend"
	"github.com/acorndb/prism/pkg/execute"
	"github.com/acorndb/prism/pkg/plan"
	"github.com/acorndb/prism/pkg/proxy"
	"github.com/acorndb/prism/pkg/route/source"
	"github.com/stretchr/testify/require"
	mysqltc "github.com/testcontainers/testcontainers-go/modules/mysql"
)

type mapUsers map[string]string

func (m mapUsers) Password(user string) (string, bool) {
	p, ok := m[user]
	return p, ok
}

// TestPassthroughDifferential runs CalibrationScript against the real proxy
// vs direct MySQL (A2). Replaces NullProxy with in-process Serve.
func TestPassthroughDifferential(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过需要 Docker 的透传差分")
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
	port, err := c.MappedPort(ctx, "3306")
	require.NoError(t, err)
	mysqlAddr := net.JoinHostPort(host, port.Port())

	p := backend.NewPool(backend.PoolConfig{Capacity: 4, MaxCapacity: 16, GetTimeout: 5 * time.Second},
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
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_ = proxy.Serve(serveCtx, c, proxy.SessionConfig{
					Users: mapUsers{calibUser: calibPass},
					Exec:  exec,
				})
				_ = c.Close()
			}(c)
		}
	}()

	steps := CalibrationScript()

	direct, err := RunScript(ctx, dsn(mysqlAddr, dbDirect), steps)
	require.NoError(t, err, "直连侧脚本执行失败")
	proxied, err := RunScript(ctx, dsn(ln.Addr().String(), dbProxied), steps)
	require.NoError(t, err, "代理侧脚本执行失败")

	scrubSchemaRefs(direct)
	scrubSchemaRefs(proxied)

	diffs := Compare(steps, direct, proxied)
	for _, d := range diffs {
		t.Log(d)
	}
	require.Empty(t, diffs, "透传差分失败: 存在 %d 处差异", len(diffs))
}
