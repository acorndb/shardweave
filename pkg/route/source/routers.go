package source

import (
	"context"
	"fmt"

	"github.com/acorndb/prism/pkg/constant"
	"github.com/acorndb/prism/pkg/route"
	"github.com/acorndb/prism/pkg/session"
	"github.com/acorndb/prism/pkg/sql"
)

// AllMaster routes every statement to the master of the default slice.
type AllMaster struct{}

var _ route.Router = AllMaster{}

// Route implements route.Router.
func (AllMaster) Route(context.Context, *session.State, *sql.Statement) ([]route.Target, error) {
	return []route.Target{{Slice: route.DefaultSlice, Role: constant.RoleMaster}}, nil
}

// Pick routes one statement to master or slave of the default slice.
type Pick struct{}

var _ route.Router = Pick{}

// Route implements route.Router.
func (Pick) Route(_ context.Context, st *session.State, stmt *sql.Statement) ([]route.Target, error) {
	if stmt == nil {
		return nil, fmt.Errorf("source: nil statement")
	}
	return []route.Target{{
		Slice: route.DefaultSlice,
		Role:  PickRole(st, stmt.Cmd, stmt.Raw),
	}}, nil
}
