package harness

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/testcontainers/testcontainers-go"
	mysqltc "github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/network"
)

const (
	// MySQLImage is pinned to the version used by CI and local experiments.
	MySQLImage = "mysql:8.0"

	AppUser      = "app"
	AppPassword  = "app-pass-123"
	ReplUser     = "repl"
	ReplPassword = "repl-pass-123"
	Database     = "bench"

	replSourceHost = "mysql-master"
)

// Instance is one real MySQL server in the topology.
type Instance struct {
	Name      string
	ServerID  int
	addr      string
	Container *mysqltc.MySQLContainer
	DB        *sql.DB // root connection, used for administration
}

// Addr returns the host-reachable address of the instance.
func (i *Instance) Addr() string { return i.addr }

// AppDSN returns the DSN an application (or proxy) uses to connect.
func (i *Instance) AppDSN() string { return appDSN(i.addr, Database) }

// Topology is a master plus N replicas connected by real GTID replication.
type Topology struct {
	Master   *Instance
	Replicas []*Instance

	network *testcontainers.DockerNetwork
	dir     string

	closeOnce sync.Once
}

// SkipIfNoDocker skips the test when no container provider is healthy.
func SkipIfNoDocker(t *testing.T) {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
}

// StartTopology boots one master and replicas real MySQL containers and wires
// GTID replication between them. Replicas are switched to read_only after
// catch-up so misrouted writes fail loudly instead of corrupting the topology.
func StartTopology(ctx context.Context, replicas int) (*Topology, error) {
	if replicas < 1 {
		return nil, fmt.Errorf("harness: replicas must be >= 1")
	}
	nw, err := network.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("harness: create network: %w", err)
	}
	dir, err := os.MkdirTemp("", "shardweave-topology-")
	if err != nil {
		_ = nw.Remove(ctx)
		return nil, fmt.Errorf("harness: temp dir: %w", err)
	}
	tp := &Topology{network: nw, dir: dir}
	ok := false
	defer func() {
		if !ok {
			closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			tp.Close(closeCtx)
		}
	}()

	masterCNF := filepath.Join(dir, "master.cnf")
	if err := os.WriteFile(masterCNF, []byte(replicationCNF(1, false)), 0o644); err != nil {
		return nil, err
	}
	masterCtr, err := startContainer(ctx, nw, replSourceHost, masterCNF, AppUser)
	if err != nil {
		return nil, fmt.Errorf("harness: start master: %w", err)
	}
	masterAddr, err := masterCtr.PortEndpoint(ctx, "3306/tcp", "")
	if err != nil {
		return nil, err
	}
	masterDB, err := openRoot(ctx, masterAddr)
	if err != nil {
		return nil, fmt.Errorf("harness: connect master: %w", err)
	}
	tp.Master = &Instance{Name: "master", ServerID: 1, addr: masterAddr, Container: masterCtr, DB: masterDB}

	if err := adminExec(ctx, masterDB,
		fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED WITH mysql_native_password BY '%s'", ReplUser, ReplPassword),
		fmt.Sprintf("GRANT REPLICATION SLAVE ON *.* TO '%s'@'%%'", ReplUser),
	); err != nil {
		return nil, fmt.Errorf("harness: create replication user: %w", err)
	}

	for i := 0; i < replicas; i++ {
		id := i + 1
		serverID := i + 2
		cnf := filepath.Join(dir, fmt.Sprintf("replica-%d.cnf", id))
		if err := os.WriteFile(cnf, []byte(replicationCNF(serverID, true)), 0o644); err != nil {
			return nil, err
		}
		ctr, err := startContainer(ctx, nw, fmt.Sprintf("mysql-replica-%d", id), cnf, "root")
		if err != nil {
			return nil, fmt.Errorf("harness: start replica-%d: %w", id, err)
		}
		addr, err := ctr.PortEndpoint(ctx, "3306/tcp", "")
		if err != nil {
			return nil, err
		}
		db, err := openRoot(ctx, addr)
		if err != nil {
			return nil, fmt.Errorf("harness: connect replica-%d: %w", id, err)
		}
		inst := &Instance{Name: fmt.Sprintf("replica-%d", id), ServerID: serverID, addr: addr, Container: ctr, DB: db}
		tp.Replicas = append(tp.Replicas, inst)

		change := fmt.Sprintf(
			"CHANGE REPLICATION SOURCE TO SOURCE_HOST='%s', SOURCE_PORT=3306, SOURCE_USER='%s', SOURCE_PASSWORD='%s', SOURCE_AUTO_POSITION=1, GET_SOURCE_PUBLIC_KEY=1",
			replSourceHost, ReplUser, ReplPassword,
		)
		if err := adminExec(ctx, db, change, "START REPLICA"); err != nil {
			return nil, fmt.Errorf("harness: start replication on %s: %w", inst.Name, err)
		}
	}

	for _, r := range tp.Replicas {
		if err := waitReplicaLinks(ctx, r); err != nil {
			return nil, err
		}
	}
	// Initial catch-up: app user and schema created on the master reach replicas
	// through the real replication stream (not local init).
	if err := tp.Sync(ctx); err != nil {
		return nil, err
	}
	for _, r := range tp.Replicas {
		if _, err := r.DB.ExecContext(ctx, "SET GLOBAL read_only = ON"); err != nil {
			return nil, fmt.Errorf("harness: set %s read_only: %w", r.Name, err)
		}
	}
	ok = true
	return tp, nil
}

// Sync waits until every replica has executed the master's current GTID set.
func (tp *Topology) Sync(ctx context.Context) error {
	if tp == nil || tp.Master == nil {
		return fmt.Errorf("harness: nil topology")
	}
	return syncReplicas(ctx, tp.Master, tp.Replicas)
}

// syncReplicas waits until every replica executed the master's GTID set.
func syncReplicas(ctx context.Context, master *Instance, replicas []*Instance) error {
	if len(replicas) == 0 {
		return nil
	}
	var gtid string
	if err := master.DB.QueryRowContext(ctx, "SELECT @@GLOBAL.gtid_executed").Scan(&gtid); err != nil {
		return fmt.Errorf("harness: read master gtid_executed: %w", err)
	}
	for _, r := range replicas {
		var timedOut int
		if err := r.DB.QueryRowContext(ctx, "SELECT WAIT_FOR_EXECUTED_GTID_SET(?, 30)", gtid).Scan(&timedOut); err != nil {
			return fmt.Errorf("harness: wait %s: %w", r.Name, err)
		}
		if timedOut != 0 {
			_, _, lastErr, _ := replicaStatus(ctx, r.DB)
			return fmt.Errorf("harness: %s did not catch up in 30s (last_error=%q)", r.Name, lastErr)
		}
	}
	return nil
}

// ReplicaServerIDs returns the configured server-id of every replica.
func (tp *Topology) ReplicaServerIDs() []int {
	if tp == nil {
		return nil
	}
	ids := make([]int, 0, len(tp.Replicas))
	for _, r := range tp.Replicas {
		ids = append(ids, r.ServerID)
	}
	return ids
}

// Close terminates containers, removes the network and temp files.
func (tp *Topology) Close(ctx context.Context) {
	if tp == nil {
		return
	}
	tp.closeOnce.Do(func() {
		for _, r := range tp.Replicas {
			closeInstance(ctx, r)
		}
		closeInstance(ctx, tp.Master)
		if tp.network != nil {
			_ = tp.network.Remove(ctx)
		}
		if tp.dir != "" {
			_ = os.RemoveAll(tp.dir)
		}
	})
}

func closeInstance(ctx context.Context, inst *Instance) {
	if inst == nil {
		return
	}
	if inst.DB != nil {
		_ = inst.DB.Close()
	}
	if inst.Container != nil {
		_ = inst.Container.Terminate(ctx)
	}
}

func startContainer(ctx context.Context, nw *testcontainers.DockerNetwork, alias, cnf, user string) (*mysqltc.MySQLContainer, error) {
	return mysqltc.Run(ctx, MySQLImage,
		mysqltc.WithUsername(user),
		mysqltc.WithPassword(AppPassword),
		mysqltc.WithDatabase(Database),
		mysqltc.WithConfigFile(cnf),
		testcontainers.WithEnv(map[string]string{"MYSQL_ROOT_HOST": "%"}),
		network.WithNetwork([]string{alias}, nw),
	)
}

func openRoot(ctx context.Context, addr string) (*sql.DB, error) {
	dsn := fmt.Sprintf("root:%s@tcp(%s)/%s?parseTime=true&timeout=10s&readTimeout=40s&writeTimeout=40s", AppPassword, addr, Database)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for {
		if err := db.PingContext(ctx); err == nil {
			return db, nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			_ = db.Close()
			return nil, fmt.Errorf("mysql %s not reachable: %w", addr, lastErr)
		}
		select {
		case <-ctx.Done():
			_ = db.Close()
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func adminExec(ctx context.Context, db *sql.DB, stmts ...string) error {
	for i, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			// Do not echo the statement: CREATE USER carries the replication password.
			return fmt.Errorf("admin statement %d: %w", i+1, err)
		}
	}
	return nil
}

func waitReplicaLinks(ctx context.Context, inst *Instance) error {
	deadline := time.Now().Add(60 * time.Second)
	var statusErr error
	for {
		ioState, sqlState, lastErr, err := replicaStatus(ctx, inst.DB)
		if err == nil && ioState == "Yes" && sqlState == "Yes" {
			return nil
		}
		if err == nil && lastErr != "" {
			return fmt.Errorf("harness: %s replication error: %s", inst.Name, lastErr)
		}
		if err != nil {
			statusErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("harness: %s replication not running (io=%s sql=%s status_err=%v)", inst.Name, ioState, sqlState, statusErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func replicaStatus(ctx context.Context, db *sql.DB) (ioState, sqlState, lastErr string, err error) {
	rows, err := db.QueryContext(ctx, "SHOW REPLICA STATUS")
	if err != nil {
		return "", "", "", err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return "", "", "", err
	}
	if !rows.Next() {
		return "", "", "", fmt.Errorf("replication not configured")
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return "", "", "", err
	}
	for i, name := range cols {
		switch name {
		case "Replica_IO_Running":
			ioState = vals[i].String
		case "Replica_SQL_Running":
			sqlState = vals[i].String
		case "Last_Error":
			lastErr = vals[i].String
		}
	}
	return ioState, sqlState, lastErr, nil
}

// replicationCNF returns the mysqld config shared by every topology node.
// default-authentication-plugin keeps handshake auth on mysql_native_password
// until the proxy gains caching_sha2 full-auth support.
func replicationCNF(serverID int, replica bool) string {
	cnf := fmt.Sprintf(`[mysqld]
server-id=%d
log-bin=mysql-bin
binlog-format=ROW
gtid-mode=ON
enforce-gtid-consistency=ON
default-authentication-plugin=mysql_native_password
`, serverID)
	if replica {
		cnf += "relay-log=relay-bin\n"
	}
	return cnf
}

func appDSN(addr, db string) string {
	return fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&timeout=10s&readTimeout=40s&writeTimeout=40s",
		AppUser, AppPassword, addr, db)
}
