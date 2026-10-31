package harness

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReplicationCNF(t *testing.T) {
	tests := []struct {
		name     string
		serverID int
		replica  bool
		want     []string
		absent   []string
	}{
		{
			name:     "master",
			serverID: 1,
			replica:  false,
			want:     []string{"server-id=1", "log-bin=mysql-bin", "gtid-mode=ON", "enforce-gtid-consistency=ON", "binlog-format=ROW"},
			absent:   []string{"relay-log"},
		},
		{
			name:     "replica",
			serverID: 3,
			replica:  true,
			want:     []string{"server-id=3", "log-bin=mysql-bin", "gtid-mode=ON", "relay-log=relay-bin"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cnf := replicationCNF(tc.serverID, tc.replica)
			for _, want := range tc.want {
				require.Contains(t, cnf, want)
			}
			for _, absent := range tc.absent {
				require.NotContains(t, cnf, absent)
			}
			require.True(t, strings.HasPrefix(cnf, "[mysqld]"))
		})
	}
}

func TestAppDSN(t *testing.T) {
	dsn := appDSN("127.0.0.1:3306", "bench")
	require.Contains(t, dsn, AppUser+":"+AppPassword+"@tcp(127.0.0.1:3306)/bench")
	require.Contains(t, dsn, "parseTime=true")
}

func TestReplicaIDs(t *testing.T) {
	tp := &Topology{
		Master: &Instance{Name: "master", ServerID: 1},
		Replicas: []*Instance{
			{Name: "replica-1", ServerID: 2},
			{Name: "replica-2", ServerID: 3},
		},
	}
	require.ElementsMatch(t, []int{2, 3}, tp.ReplicaServerIDs())
}
