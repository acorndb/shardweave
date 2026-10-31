package rwsplit_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"

	"github.com/acorndb/shardweave/test/harness"
	"github.com/acorndb/shardweave/test/randgen"
)

// runRandomizedSelectDifferential executes seeded random SELECTs against the
// master and through the proxy, then compares columns, rows and errors.
// Rows are canonicalized (sorted) because SQL ORDER BY is not the property
// under test here; the read path forwarding is.
func runRandomizedSelectDifferential(t *testing.T, ctx context.Context, tp *harness.Topology, proxy *harness.Proxy) {
	t.Helper()
	seedRandomSchema(t, ctx, tp)

	db, err := sql.Open("mysql", proxy.DSN())
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	queries := envInt(t, "SHARDWEAVE_RANDOM_QUERIES", 120)
	seed := int64(envInt(t, "SHARDWEAVE_RANDOM_SEED", 20261031))
	gen := randgen.New(seed, randomSchema())

	nonEmpty := 0
	for i := 0; i < queries; i++ {
		q := gen.Select()
		direct := observeQuery(ctx, tp.Master.DB, q)
		require.NoErrorf(t, direct.err, "生成器产出非法 SQL（i=%d, seed=%d）: %s", i, seed, q)
		if len(direct.rows) > 0 {
			nonEmpty++
		}

		proxied := observeQuery(ctx, db, q)
		if !direct.equal(proxied) {
			t.Fatalf("随机查询不一致（i=%d, seed=%d）:\nSQL: %s\n直连: %s\n代理: %s", i, seed, q, direct, proxied)
		}
	}
	require.Greater(t, nonEmpty, queries/3, "非空结果太少，随机查询没有覆盖到结果集比较")
	t.Logf("randomized SELECT differential: %d 条查询, seed=%d, 非空结果 %d", queries, seed, nonEmpty)
}

type queryObs struct {
	columns []string
	rows    [][]string
	err     error
	errNo   uint16
	errMsg  string
}

func (o queryObs) String() string {
	if o.err != nil {
		return fmt.Sprintf("errno=%d msg=%q", o.errNo, o.errMsg)
	}
	return fmt.Sprintf("cols=%v rows=%v", o.columns, o.rows)
}

// equal compares observable behavior, not concrete error wrappers.
func (o queryObs) equal(other queryObs) bool {
	if o.err != nil || other.err != nil {
		return o.err != nil && other.err != nil &&
			o.errNo == other.errNo && o.errMsg == other.errMsg
	}
	return reflect.DeepEqual(o.columns, other.columns) &&
		reflect.DeepEqual(o.rows, other.rows)
}

func observeQuery(ctx context.Context, db *sql.DB, query string) queryObs {
	return observeQueryArgs(ctx, db, query)
}

func observeQueryArgs(ctx context.Context, db *sql.DB, query string, args ...any) queryObs {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return queryObs{err: err, errNo: mysqlErrNo(err), errMsg: err.Error()}
	}
	defer rows.Close()

	obs := queryObs{}
	obs.columns, err = rows.Columns()
	if err != nil {
		obs.err = err
		obs.errMsg = err.Error()
		return obs
	}
	values := make([]any, len(obs.columns))
	ptrs := make([]any, len(obs.columns))
	for i := range values {
		ptrs[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			obs.err = err
			obs.errMsg = err.Error()
			return obs
		}
		row := make([]string, len(values))
		for i, v := range values {
			row[i] = formatValue(v)
		}
		obs.rows = append(obs.rows, row)
	}
	if err := rows.Err(); err != nil {
		obs.err = err
		obs.errNo = mysqlErrNo(err)
		obs.errMsg = err.Error()
		return obs
	}
	sort.Slice(obs.rows, func(i, j int) bool {
		return fmt.Sprint(obs.rows[i]) < fmt.Sprint(obs.rows[j])
	})
	return obs
}

func formatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(x)
	case time.Time:
		return x.Format("2006-01-02 15:04:05.999999")
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func mysqlErrNo(err error) uint16 {
	var me *drivermysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	require.NoError(t, err, "%s 必须是整数", key)
	if n <= 0 {
		return def
	}
	return n
}

func seedRandomSchema(t *testing.T, ctx context.Context, tp *harness.Topology) {
	t.Helper()
	stmts := []string{
		"DROP TABLE IF EXISTS rnd_int",
		"DROP TABLE IF EXISTS rnd_str",
		"DROP TABLE IF EXISTS rnd_null",
		`CREATE TABLE rnd_int (
			id INT NOT NULL PRIMARY KEY,
			a INT NOT NULL,
			b BIGINT NOT NULL,
			c DECIMAL(10,2) NOT NULL
		)`,
		`CREATE TABLE rnd_str (
			id INT NOT NULL PRIMARY KEY,
			s VARCHAR(32) NOT NULL,
			t VARCHAR(64) NOT NULL,
			d DATE NOT NULL,
			dt DATETIME NOT NULL,
			f DOUBLE NOT NULL
		)`,
		`CREATE TABLE rnd_null (
			id INT NOT NULL PRIMARY KEY,
			v INT NULL,
			s VARCHAR(16) NULL
		)`,
	}
	for _, stmt := range stmts {
		_, err := tp.Master.DB.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}

	tx, err := tp.Master.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 20; i++ {
		_, err = tx.ExecContext(ctx,
			"INSERT INTO rnd_int (id, a, b, c) VALUES (?, ?, ?, ?)",
			i, i*3-7, int64(i)*1000, float64(i)/2)
		require.NoError(t, err)

		day := base.AddDate(0, 0, i)
		_, err = tx.ExecContext(ctx,
			"INSERT INTO rnd_str (id, s, t, d, dt, f) VALUES (?, ?, ?, ?, ?, ?)",
			i, fmt.Sprintf("s%02d", i), fmt.Sprintf("text-%02d", i), day, day.Add(time.Duration(i)*time.Hour), float64(i)*1.5)
		require.NoError(t, err)

		var v any
		if i%2 == 0 {
			v = nil
		} else {
			v = i
		}
		var s any
		if i%3 == 0 {
			s = nil
		} else {
			s = fmt.Sprintf("n%02d", i)
		}
		_, err = tx.ExecContext(ctx,
			"INSERT INTO rnd_null (id, v, s) VALUES (?, ?, ?)", i, v, s)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())
	require.NoError(t, tp.Sync(ctx))
}

func randomSchema() []randgen.Table {
	return []randgen.Table{
		{Name: "rnd_int", Columns: []randgen.Column{
			{Name: "id", Kind: randgen.Int},
			{Name: "a", Kind: randgen.Int},
			{Name: "b", Kind: randgen.Int},
			{Name: "c", Kind: randgen.Float},
		}},
		{Name: "rnd_str", Columns: []randgen.Column{
			{Name: "id", Kind: randgen.Int},
			{Name: "s", Kind: randgen.Text},
			{Name: "t", Kind: randgen.Text},
			{Name: "d", Kind: randgen.Date},
			{Name: "dt", Kind: randgen.Date},
			{Name: "f", Kind: randgen.Float},
		}},
		{Name: "rnd_null", Columns: []randgen.Column{
			{Name: "id", Kind: randgen.Int},
			{Name: "v", Kind: randgen.Int},
			{Name: "s", Kind: randgen.Text},
		}},
	}
}
