package passthrough_test

import (
	"context"
	"database/sql"
	"net"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/acorndb/prism/pkg/pool"
	"github.com/acorndb/prism/pkg/proxy"
	"github.com/stretchr/testify/require"
	mysqltc "github.com/testcontainers/testcontainers-go/modules/mysql"
)

type mapUsers map[string]string

func (m mapUsers) Password(user string) (string, bool) {
	p, ok := m[user]
	return p, ok
}

func TestPassthrough_Select1(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过需要 Docker 的透传冒烟")
	}

	ctx := context.Background()
	cfg, err := filepath.Abs(filepath.Join("testdata", "native.cnf"))
	require.NoError(t, err)

	mc, err := mysqltc.Run(ctx, "mysql:8.0",
		mysqltc.WithDatabase("smoke"),
		mysqltc.WithUsername("smoke"),
		mysqltc.WithPassword("smoke-pass"),
		mysqltc.WithConfigFile(cfg),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mc.Terminate(ctx) })

	host, err := mc.Host(ctx)
	require.NoError(t, err)
	port, err := mc.MappedPort(ctx, "3306")
	require.NoError(t, err)
	mysqlAddr := net.JoinHostPort(host, port.Port())

	p := pool.New(pool.Config{Capacity: 1, MaxCapacity: 4, GetTimeout: 5 * time.Second},
		func(ctx context.Context) (pool.Backend, error) {
			return pool.DialMySQL(ctx, mysqlAddr, "smoke", "smoke-pass", "smoke")
		})
	t.Cleanup(func() { _ = p.Close() })

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
					Users: mapUsers{"smoke": "smoke-pass"},
					Pool:  p,
				})
				_ = c.Close()
			}(c)
		}
	}()

	dsn := "smoke:smoke-pass@tcp(" + ln.Addr().String() + ")/smoke?timeout=5s&parseTime=true"
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var deadline = time.Now().Add(30 * time.Second)
	var n int
	for {
		err = db.QueryRowContext(ctx, "SELECT 1").Scan(&n)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			require.NoError(t, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	require.Equal(t, 1, n)
}
