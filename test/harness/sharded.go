package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"

	"github.com/acorndb/shardweave/pkg/config"
	"github.com/acorndb/shardweave/pkg/constant"
)

// ShardedTopology is N independent MySQL slices. Each slice can optionally have
// its own replicas connected by GTID replication; slices never replicate with
// each other.
type ShardedTopology struct {
	Slices   []*Instance   // slice masters, in config order
	Replicas [][]*Instance // per-slice replicas; index matches Slices

	network *testcontainers.DockerNetwork
	dir     string

	closeOnce sync.Once
}

// StartShardedTopology boots n independent master-only MySQL containers.
func StartShardedTopology(ctx context.Context, n int) (*ShardedTopology, error) {
	return StartShardedTopologyWithReplicas(ctx, n, 0)
}

// StartShardedTopologyWithReplicas boots n independent slices, each with a
// master and the given number of GTID replicas. Replicas are read_only after
// catch-up so misrouted writes fail loudly.
func StartShardedTopologyWithReplicas(ctx context.Context, n, replicas int) (*ShardedTopology, error) {
	if n < 1 {
		return nil, fmt.Errorf("harness: shards must be >= 1")
	}
	if replicas < 0 {
		return nil, fmt.Errorf("harness: replicas must be >= 0")
	}
	nw, err := network.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("harness: create network: %w", err)
	}
	dir, err := os.MkdirTemp("", "shardweave-sharded-")
	if err != nil {
		_ = nw.Remove(ctx)
		return nil, fmt.Errorf("harness: temp dir: %w", err)
	}
	tp := &ShardedTopology{network: nw, dir: dir, Replicas: make([][]*Instance, n)}
	ok := false
	defer func() {
		if !ok {
			closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			tp.Close(closeCtx)
		}
	}()

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("s%d", i)
		masterAlias := fmt.Sprintf("mysql-%s-master", name)
		serverID := i*(replicas+1) + 1

		masterCNF := filepath.Join(dir, name+"-master.cnf")
		if err := os.WriteFile(masterCNF, []byte(replicationCNF(serverID, false)), 0o644); err != nil {
			return nil, err
		}
		masterCtr, err := startContainer(ctx, nw, masterAlias, masterCNF, AppUser)
		if err != nil {
			return nil, fmt.Errorf("harness: start %s master: %w", name, err)
		}
		addr, err := masterCtr.PortEndpoint(ctx, "3306/tcp", "")
		if err != nil {
			return nil, err
		}
		db, err := openRoot(ctx, addr)
		if err != nil {
			return nil, fmt.Errorf("harness: connect %s master: %w", name, err)
		}
		tp.Slices = append(tp.Slices, &Instance{
			Name: name, ServerID: serverID, addr: addr, Container: masterCtr, DB: db,
		})

		if err := adminExec(ctx, db,
			fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED WITH mysql_native_password BY '%s'", ReplUser, ReplPassword),
			fmt.Sprintf("GRANT REPLICATION SLAVE ON *.* TO '%s'@'%%'", ReplUser),
		); err != nil {
			return nil, fmt.Errorf("harness: create %s replication user: %w", name, err)
		}

		for j := 0; j < replicas; j++ {
			replicaName := fmt.Sprintf("%s-replica-%d", name, j+1)
			replicaAlias := fmt.Sprintf("mysql-%s", replicaName)
			replicaID := serverID + 1 + j
			cnf := filepath.Join(dir, replicaName+".cnf")
			if err := os.WriteFile(cnf, []byte(replicationCNF(replicaID, true)), 0o644); err != nil {
				return nil, err
			}
			ctr, err := startContainer(ctx, nw, replicaAlias, cnf, "root")
			if err != nil {
				return nil, fmt.Errorf("harness: start %s: %w", replicaName, err)
			}
			replicaAddr, err := ctr.PortEndpoint(ctx, "3306/tcp", "")
			if err != nil {
				return nil, err
			}
			replicaDB, err := openRoot(ctx, replicaAddr)
			if err != nil {
				return nil, fmt.Errorf("harness: connect %s: %w", replicaName, err)
			}
			inst := &Instance{Name: replicaName, ServerID: replicaID, addr: replicaAddr, Container: ctr, DB: replicaDB}
			tp.Replicas[i] = append(tp.Replicas[i], inst)

			change := fmt.Sprintf(
				"CHANGE REPLICATION SOURCE TO SOURCE_HOST='%s', SOURCE_PORT=3306, SOURCE_USER='%s', SOURCE_PASSWORD='%s', SOURCE_AUTO_POSITION=1, GET_SOURCE_PUBLIC_KEY=1",
				masterAlias, ReplUser, ReplPassword,
			)
			if err := adminExec(ctx, replicaDB, change, "START REPLICA"); err != nil {
				return nil, fmt.Errorf("harness: start replication on %s: %w", replicaName, err)
			}
		}
	}

	for i, master := range tp.Slices {
		for _, r := range tp.Replicas[i] {
			if err := waitReplicaLinks(ctx, r); err != nil {
				return nil, err
			}
		}
		if err := syncReplicas(ctx, master, tp.Replicas[i]); err != nil {
			return nil, err
		}
		for _, r := range tp.Replicas[i] {
			if _, err := r.DB.ExecContext(ctx, "SET GLOBAL read_only = ON"); err != nil {
				return nil, fmt.Errorf("harness: set %s read_only: %w", r.Name, err)
			}
		}
	}
	ok = true
	return tp, nil
}

// Sync waits until every replica of every slice catches up with its master.
func (tp *ShardedTopology) Sync(ctx context.Context) error {
	if tp == nil {
		return fmt.Errorf("harness: nil sharded topology")
	}
	for i, master := range tp.Slices {
		if err := syncReplicas(ctx, master, tp.Replicas[i]); err != nil {
			return fmt.Errorf("harness: shard %s: %w", master.Name, err)
		}
	}
	return nil
}

// Slice returns the named slice master.
func (tp *ShardedTopology) Slice(name string) (*Instance, bool) {
	for _, s := range tp.Slices {
		if s.Name == name {
			return s, true
		}
	}
	return nil, false
}

// ProxyConfig builds a sharding proxy config with rules for orders(user_id)
// and profiles(name) over all slices in configuration order.
func (tp *ShardedTopology) ProxyConfig() *config.Config {
	names := make([]string, 0, len(tp.Slices))
	slices := make([]config.SliceConfig, 0, len(tp.Slices))
	for i, s := range tp.Slices {
		names = append(names, s.Name)
		sc := config.SliceConfig{
			Name: s.Name,
			Master: config.BackendEndpoint{
				Address: s.Addr(), User: AppUser, Password: AppPassword, Database: Database,
			},
		}
		for _, r := range tp.Replicas[i] {
			sc.Slaves = append(sc.Slaves, config.BackendEndpoint{
				Address: r.Addr(), User: AppUser, Password: AppPassword, Database: Database,
			})
		}
		slices = append(slices, sc)
	}
	return &config.Config{
		Listen: "127.0.0.1:0",
		Users:  []config.User{{Name: AppUser, Password: AppPassword}},
		Server: config.Server{MaxAllowedPacket: 0},
		Backend: config.BackendConfig{
			Mode:        constant.ModeSharding,
			LoadBalance: constant.LoadBalanceRoundRobin,
			Slices:      slices,
			Sharding: &config.ShardingConfig{Rules: []config.ShardRule{
				{
					Table:     "orders",
					Key:       "user_id",
					Algorithm: constant.ShardingHashMod,
					Slices:    names,
				},
				{
					Table:     "profiles",
					Key:       "name",
					Algorithm: constant.ShardingHashModStr,
					Slices:    names,
				},
			}},
		},
		Pool: config.Pool{
			Capacity:       0,
			MaxCapacity:    8,
			MaxIdle:        4,
			AcquireTimeout: 5 * time.Second,
			DialTimeout:    3 * time.Second,
			ReadTimeout:    15 * time.Second,
			WriteTimeout:   15 * time.Second,
		},
	}
}

// Close terminates every slice container and removes the network/temp dir.
func (tp *ShardedTopology) Close(ctx context.Context) {
	if tp == nil {
		return
	}
	tp.closeOnce.Do(func() {
		for i, s := range tp.Slices {
			for _, r := range tp.Replicas[i] {
				closeInstance(ctx, r)
			}
			closeInstance(ctx, s)
		}
		if tp.network != nil {
			_ = tp.network.Remove(ctx)
		}
		if tp.dir != "" {
			_ = os.RemoveAll(tp.dir)
		}
	})
}
