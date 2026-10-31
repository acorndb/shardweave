// Package plan builds execution plans from statements and routing decisions.
package plan

import (
	"context"
	"fmt"

	"github.com/acorndb/shardweave/pkg/route"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
)

// Kind is the three-state plan contract.
type Kind uint8

const (
	Passthrough Kind = iota
	SingleShard
	MultiShard
	Reject
)

func (k Kind) String() string {
	switch k {
	case Passthrough:
		return "Passthrough"
	case SingleShard:
		return "SingleShard"
	case MultiShard:
		return "MultiShard"
	case Reject:
		return "Reject"
	default:
		return "Unknown"
	}
}

// MergeSpec is the merge hint for MultiShard plans. P3 uses the zero value.
type MergeSpec struct{}

// RejectError is a stable, documented rejection. P3 defines the final codes.
type RejectError struct {
	Code    uint16
	State   string
	Message string
}

func (e *RejectError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("plan reject: %s", e.Message)
}

// Plan is immutable after Build.
type Plan struct {
	Kind    Kind
	Targets []route.Target
	Merge   MergeSpec
	Reject  *RejectError
}

// Builder composes a Router (and later a Rewriter) into a Plan.
type Builder struct {
	Router route.Router
}

// Build produces a one-target Passthrough plan for single/rwsplit.
// MultiShard arrives in P4; until then more than one target is rejected.
func (b Builder) Build(ctx context.Context, st *session.State, stmt *sql.Statement) (*Plan, error) {
	if b.Router == nil {
		return nil, fmt.Errorf("plan: nil router")
	}
	if stmt == nil {
		return nil, fmt.Errorf("plan: nil statement")
	}
	targets, err := b.Router.Route(ctx, st, stmt)
	if err != nil {
		return nil, err
	}
	switch len(targets) {
	case 0:
		return nil, fmt.Errorf("plan: router returned no targets")
	case 1:
		return &Plan{Kind: Passthrough, Targets: targets}, nil
	default:
		return &Plan{
			Kind: Reject,
			Reject: &RejectError{
				Code:    1105,
				State:   "HY000",
				Message: "multi-target plan not implemented",
			},
		}, nil
	}
}
