// Package route defines routing targets and the Router contract.
package route

import (
	"context"

	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
)

// DefaultSlice is the logical slice for single/rwsplit topologies.
const DefaultSlice = "default"

// Target is one backend destination for a statement.
// DB is the physical schema; session.State only holds the logical database.
type Target struct {
	Slice string
	Role  constant.Role
	DB    string
	SQL   string
}

// Router turns one statement into a set of targets. Implementations are pure:
// they must not acquire connections or hold pools.
type Router interface {
	Route(ctx context.Context, st *session.State, stmt *sql.Statement) ([]Target, error)
}
