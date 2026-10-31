package source

import (
	"context"
	"fmt"

	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/acorndb/shardweave/pkg/route"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
)

// AllMaster routes every statement to the master of a slice.
type AllMaster struct {
	Slice string // empty → route.DefaultSlice
}

var _ route.Router = AllMaster{}

// Route implements route.Router.
func (r AllMaster) Route(context.Context, *session.State, *sql.Statement) ([]route.Target, error) {
	return []route.Target{{Slice: r.sliceName(), Role: constant.RoleMaster}}, nil
}

// Pick routes one statement to master or slave of a slice.
type Pick struct {
	Slice string // empty → route.DefaultSlice
}

var _ route.Router = Pick{}

func (r AllMaster) sliceName() string {
	if r.Slice == "" {
		return route.DefaultSlice
	}
	return r.Slice
}

func (r Pick) sliceName() string {
	if r.Slice == "" {
		return route.DefaultSlice
	}
	return r.Slice
}

// Route implements route.Router.
func (r Pick) Route(_ context.Context, st *session.State, stmt *sql.Statement) ([]route.Target, error) {
	if stmt == nil {
		return nil, fmt.Errorf("source: nil statement")
	}
	return []route.Target{{
		Slice: r.sliceName(),
		Role:  PickRole(st, stmt.Cmd, stmt.Raw),
	}}, nil
}
