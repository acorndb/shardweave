// Package gorm_test exercises shardweave from an ORM/SDK perspective: a real
// GORM client connects through the proxy and runs CRUD, transactions and
// prepared statements against real MySQL.
package gorm_test

import (
	"context"
	"errors"
	"testing"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/acorndb/shardweave/test/harness"
)

type gormUser struct {
	ID   uint64 `gorm:"primaryKey"`
	Name string `gorm:"size:32;not null"`
	Age  int    `gorm:"not null"`
}

func (gormUser) TableName() string { return "gorm_users" }

// TestGORM_RWSplitCRUD drives the proxy with GORM in rwsplit mode: schema DDL
// goes to the master, reads are served by the replica, and every write path
// (including GORM's default transaction wrapper and prepared statements) must
// round-trip correctly.
func TestGORM_RWSplitCRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过需要 Docker 的 GORM 集成测试")
	}
	harness.SkipIfNoDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	tp, err := harness.StartTopology(ctx, 1)
	require.NoError(t, err)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer closeCancel()
		tp.Close(closeCtx)
	})

	_, err = tp.Master.DB.ExecContext(ctx, `CREATE TABLE gorm_users (
		id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
		name VARCHAR(32) NOT NULL,
		age INT NOT NULL
	) ENGINE=InnoDB`)
	require.NoError(t, err)
	require.NoError(t, tp.Sync(ctx), "replica must see the new table")

	proxy, err := harness.StartProxy(tp.ProxyConfig())
	require.NoError(t, err)
	t.Cleanup(proxy.Close)

	db, err := gorm.Open(gormmysql.Open(proxy.DSN()), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	// Create: GORM wraps this in BEGIN/INSERT/COMMIT by default, so this also
	// covers the proxy's transaction statement path.
	alice := gormUser{Name: "alice", Age: 30}
	require.NoError(t, db.Create(&alice).Error)
	require.NotZero(t, alice.ID)

	var got gormUser
	require.NoError(t, db.First(&got, alice.ID).Error)
	require.Equal(t, "alice", got.Name)
	require.Equal(t, 30, got.Age)

	require.NoError(t, db.Model(&got).Update("age", 31).Error)
	require.NoError(t, db.First(&got, alice.ID).Error)
	require.Equal(t, 31, got.Age)

	// Rollback: the row written inside the transaction must disappear.
	rollback := errors.New("rollback on purpose")
	err = db.Transaction(func(tx *gorm.DB) error {
		require.NoError(t, tx.Create(&gormUser{Name: "bob", Age: 20}).Error)
		return rollback
	})
	require.ErrorIs(t, err, rollback)

	var bobCount int64
	require.NoError(t, db.Model(&gormUser{}).Where("name = ?", "bob").Count(&bobCount).Error)
	require.Zero(t, bobCount)

	// Commit: a committed transaction survives.
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return tx.Create(&gormUser{Name: "carol", Age: 25}).Error
	}))

	// Plain COM_QUERY path (no bind args) reads from the replica; wait for
	// replication before asserting.
	require.NoError(t, tp.Sync(ctx))
	var total int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM gorm_users").Scan(&total).Error)
	require.EqualValues(t, 2, total)

	require.NoError(t, db.Delete(&gormUser{}, alice.ID).Error)
	var masterCount int64
	require.NoError(t, tp.Master.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM gorm_users").Scan(&masterCount))
	require.EqualValues(t, 1, masterCount, "only carol must remain")
}

type shardedOrder struct {
	ID     int64 `gorm:"primaryKey"`
	UserID int   `gorm:"column:user_id;not null"`
	Amount int   `gorm:"not null"`
}

func (shardedOrder) TableName() string { return "orders" }

// TestGORM_ShardingConstraints documents what a stock GORM client can and
// cannot do through sharding mode:
//
//   - shard-key routing only works with literal values in the SQL text, so the
//     client must set interpolateParams=true; otherwise go-sql-driver prepares
//     the statement and the proxy rejects an unresolved shard key with 1235;
//   - sharding mode rejects transaction statements, so GORM's default
//     BEGIN/COMMIT wrapper must be disabled with SkipDefaultTransaction.
func TestGORM_ShardingConstraints(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过需要 Docker 的 GORM 集成测试")
	}
	harness.SkipIfNoDocker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	tp, err := harness.StartShardedTopology(ctx, 4)
	require.NoError(t, err)
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer closeCancel()
		tp.Close(closeCtx)
	})

	for _, s := range tp.Slices {
		_, err := s.DB.ExecContext(ctx, `CREATE TABLE orders (
			id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			user_id INT NOT NULL,
			amount INT NOT NULL
		)`)
		require.NoError(t, err)
	}

	proxy, err := harness.StartProxy(tp.ProxyConfig())
	require.NoError(t, err)
	t.Cleanup(proxy.Close)

	cfg := &gorm.Config{
		SkipDefaultTransaction: true,
		Logger:                 logger.Default.LogMode(logger.Silent),
	}
	// Recommended client configuration for sharding mode.
	interp, err := gorm.Open(gormmysql.Open(proxy.DSN()+"&interpolateParams=true"), cfg)
	require.NoError(t, err)
	// Stock configuration: prepared statements + default transaction wrapper.
	stock, err := gorm.Open(gormmysql.Open(proxy.DSN()), cfg)
	require.NoError(t, err)
	// Default transaction wrapper on top of interpolation.
	txWrap, err := gorm.Open(gormmysql.Open(proxy.DSN()+"&interpolateParams=true"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)

	// user_id=2 -> s2, user_id=1 -> s1 (hash_mod over four slices).
	o := shardedOrder{UserID: 2, Amount: 100}
	require.NoError(t, interp.Create(&o).Error)
	require.NotZero(t, o.ID)
	require.NoError(t, interp.Create(&shardedOrder{UserID: 1, Amount: 50}).Error)

	var got shardedOrder
	require.NoError(t, interp.Where("user_id = ?", 2).First(&got).Error)
	require.Equal(t, 100, got.Amount)

	var all []shardedOrder
	require.NoError(t, interp.Where("amount > ?", 0).Order("id").Find(&all).Error)
	require.Len(t, all, 2, "scatter read goes through the merge path")

	// Stock GORM: INSERT is prepared, and sharding mode only supports prepared
	// SELECT.
	err = stock.Create(&shardedOrder{UserID: 2, Amount: 7}).Error
	requireMySQLCode(t, err, 1235)

	// Stock GORM: prepared SELECT with a placeholder shard key cannot be
	// routed because the literal is not visible at prepare time.
	err = stock.Where("user_id = ?", 2).First(&got).Error
	requireMySQLCode(t, err, 1235)

	// GORM's default transaction wrapper sends BEGIN, which sharding mode
	// rejects.
	err = txWrap.Create(&shardedOrder{UserID: 2, Amount: 7}).Error
	requireMySQLCode(t, err, 1235)

	// None of the rejected writes may have reached a shard.
	require.EqualValues(t, 1, shardCount(t, ctx, tp, "s2", "SELECT COUNT(*) FROM orders WHERE user_id = 2"))
	require.EqualValues(t, 0, shardCount(t, ctx, tp, "s1", "SELECT COUNT(*) FROM orders WHERE user_id = 2"))
	require.EqualValues(t, 1, shardCount(t, ctx, tp, "s1", "SELECT COUNT(*) FROM orders WHERE user_id = 1"))
	require.EqualValues(t, 0, shardCount(t, ctx, tp, "s0", "SELECT COUNT(*) FROM orders"))
	require.EqualValues(t, 0, shardCount(t, ctx, tp, "s3", "SELECT COUNT(*) FROM orders"))
}

func requireMySQLCode(t *testing.T, err error, code uint16) {
	t.Helper()
	require.Error(t, err)
	var me *drivermysql.MySQLError
	require.Truef(t, errors.As(err, &me), "want MySQL error %d, got %v", code, err)
	require.Equal(t, code, me.Number)
}

func shardCount(t *testing.T, ctx context.Context, tp *harness.ShardedTopology, name, query string) int64 {
	t.Helper()
	inst, ok := tp.Slice(name)
	require.Truef(t, ok, "unknown slice %s", name)
	var n int64
	require.NoError(t, inst.DB.QueryRowContext(ctx, query).Scan(&n))
	return n
}
