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

// ShardedTopology is N independent MySQL slices (master only in P0 tests).
type ShardedTopology struct {
	Slices []*Instance

	network *testcontainers.DockerNetwork
	dir     string

	closeOnce sync.Once
}

// StartShardedTopology boots n independent MySQL containers, one per slice.
// No replication is configured between slices.
func StartShardedTopology(ctx context.Context, n int) (*ShardedTopology, error) {
	if n < 1 {
		return nil, fmt.Errorf("harness: shards must be >= 1")
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
	tp := &ShardedTopology{network: nw, dir: dir}
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
		cnf := filepath.Join(dir, name+".cnf")
		if err := os.WriteFile(cnf, []byte(replicationCNF(i+1, false)), 0o644); err != nil {
			return nil, err
		}
		ctr, err := startContainer(ctx, nw, "mysql-"+name, cnf, AppUser)
		if err != nil {
			return nil, fmt.Errorf("harness: start %s: %w", name, err)
		}
		addr, err := ctr.PortEndpoint(ctx, "3306/tcp", "")
		if err != nil {
			return nil, err
		}
		db, err := openRoot(ctx, addr)
		if err != nil {
			return nil, fmt.Errorf("harness: connect %s: %w", name, err)
		}
		tp.Slices = append(tp.Slices, &Instance{
			Name: name, ServerID: i + 1, addr: addr, Container: ctr, DB: db,
		})
	}
	ok = true
	return tp, nil
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

// ProxyConfig builds a sharding proxy config with one rule:
// orders(user_id) split across all slices in configuration order.
func (tp *ShardedTopology) ProxyConfig() *config.Config {
	names := make([]string, 0, len(tp.Slices))
	slices := make([]config.SliceConfig, 0, len(tp.Slices))
	for _, s := range tp.Slices {
		names = append(names, s.Name)
		slices = append(slices, config.SliceConfig{
			Name: s.Name,
			Master: config.BackendEndpoint{
				Address: s.Addr(), User: AppUser, Password: AppPassword, Database: Database,
			},
		})
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
		for _, s := range tp.Slices {
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
