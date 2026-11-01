package corpus_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"

	"github.com/acorndb/shardweave/test/corpus"
)

const (
	defaultDirectDSN = "app:app-pass-123@tcp(127.0.0.1:3308)/bench?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s"
	defaultProxyDSN  = "app:app-pass-123@tcp(127.0.0.1:3307)/bench?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s"
)

// TestSQLCorpus runs testdata/sql/*.yaml against the fixed Docker fixture
// (make e2e-up MODE=single) and a direct connection to the same MySQL.
func TestSQLCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过需要 Docker 环境的 SQL 语料")
	}
	directDSN := envOr("DIRECT_DSN", defaultDirectDSN)
	proxyDSN := envOr("PROXY_DSN", defaultProxyDSN)
	if !reachable(directDSN) {
		t.Skipf("DIRECT_DSN 不可达；先 make e2e-up MODE=single (%s)", directDSN)
	}
	if !reachable(proxyDSN) {
		t.Skipf("PROXY_DSN 不可达；先 make e2e-up MODE=single (%s)", proxyDSN)
	}

	cases, err := corpus.Load("testdata/sql")
	require.NoError(t, err)
	require.NotEmpty(t, cases)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, c := range cases {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			diffs, err := corpus.Run(ctx, directDSN, proxyDSN, c)
			require.NoError(t, err)
			require.Empty(t, diffs)
		})
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func reachable(dsn string) bool {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return false
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return db.PingContext(ctx) == nil
}
