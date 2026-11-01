package mtr_test

import (
	"context"
	"database/sql"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"

	"github.com/acorndb/shardweave/test/mtr"
)

const (
	defaultDirectDSN = "app:app-pass-123@tcp(127.0.0.1:3308)/bench?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s"
	defaultProxyDSN  = "app:app-pass-123@tcp(127.0.0.1:3307)/bench?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s"
)

var update = flag.Bool("update", false, "record .result golden files from the single endpoint")

// TestMTR runs t/*.sql against the single proxy and compares r/*.result.
// With -update it records from DIRECT_DSN instead.
func TestMTR(t *testing.T) {
	runSuite(t, "t", "r",
		envOr("DIRECT_DSN", defaultDirectDSN),
		envOr("PROXY_DSN", defaultProxyDSN),
		"single")
}

// TestMTRSharding runs t_sharding/*.sql against the 4-shard proxy and compares
// r_sharding/*.result. Goldens are recorded once from the single endpoint
// (SINGLE_DSN); normal runs only touch the sharding proxy.
func TestMTRSharding(t *testing.T) {
	runSuite(t, "t_sharding", "r_sharding",
		envOr("SINGLE_DSN", defaultDirectDSN),
		envOr("PROXY_DSN", defaultProxyDSN),
		"sharding")
}

func runSuite(t *testing.T, casesDir, goldenDir, recordDSN, proxyDSN, mode string) {
	t.Helper()
	if testing.Short() {
		t.Skip("short 模式跳过需要 Docker 环境的 MTR 语料")
	}
	if *update {
		if !reachable(recordDSN) {
			t.Skipf("录制端不可达；先 make e2e-up MODE=single (%s)", recordDSN)
		}
	} else if !reachable(proxyDSN) {
		t.Skipf("PROXY_DSN 不可达；先 make e2e-up MODE=%s (%s)", mode, proxyDSN)
	}

	files, err := filepath.Glob(filepath.Join(casesDir, "*.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, file := range files {
		file := file
		name := strings.TrimSuffix(filepath.Base(file), ".sql")
		t.Run(name, func(t *testing.T) {
			script, err := mtr.ParseFile(file)
			require.NoError(t, err)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			if *update {
				got, err := mtr.Run(ctx, recordDSN, script)
				require.NoError(t, err)
				require.NoError(t, os.MkdirAll(goldenDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(goldenDir, name+".result"), []byte(got), 0o644))
				return
			}

			golden := filepath.Join(goldenDir, name+".result")
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("missing golden %s; run: go test ./test/mtr -run %s -update", golden, t.Name())
			}
			got, err := mtr.Run(ctx, proxyDSN, script)
			require.NoError(t, err)
			require.Equal(t, string(want), got)
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
