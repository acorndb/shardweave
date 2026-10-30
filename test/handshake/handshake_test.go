package handshake_test

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/acorndb/prism/mysql/client"
	"github.com/stretchr/testify/require"
	mysqltc "github.com/testcontainers/testcontainers-go/modules/mysql"
)

// TestClientDial_RealMySQL verifies P0 client handshake against MySQL 8.
// Config lives under test/handshake/testdata — not inside mysql/ packages.
func TestClientDial_RealMySQL(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过需要 Docker 的握手冒烟")
	}

	ctx := context.Background()
	cfg, err := filepath.Abs(filepath.Join("testdata", "native.cnf"))
	require.NoError(t, err)

	c, err := mysqltc.Run(ctx, "mysql:8.0",
		mysqltc.WithDatabase("smoke"),
		mysqltc.WithUsername("smoke"),
		mysqltc.WithPassword("smoke-pass"),
		mysqltc.WithConfigFile(cfg),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "3306")
	require.NoError(t, err)
	addr := net.JoinHostPort(host, port.Port())

	var raw net.Conn
	deadline := time.Now().Add(30 * time.Second)
	for {
		raw, err = net.DialTimeout("tcp", addr, 3*time.Second)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			require.NoError(t, err, "dial MySQL")
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Cleanup(func() { _ = raw.Close() })

	conn, err := client.Dial(raw, "smoke", "smoke-pass", "smoke")
	require.NoError(t, err)
	require.NotNil(t, conn)
	require.NotNil(t, conn.PacketConn())
}
