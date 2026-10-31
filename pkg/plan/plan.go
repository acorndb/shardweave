// Package plan builds execution plans from statements and routing decisions.
package plan

import (
	"context"
	"fmt"

	"github.com/acorndb/shardweave/pkg/errs"
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

// TxPolicy is the transaction constraint attached to a sharding plan.
type TxPolicy uint8

const (
	TxNone TxPolicy = iota
	TxSingleShard
	TxReject
)

func (p TxPolicy) String() string {
	switch p {
	case TxNone:
		return "TxNone"
	case TxSingleShard:
		return "TxSingleShard"
	case TxReject:
		return "TxReject"
	default:
		return "Unknown"
	}
}

// OrderKey is one ORDER BY column index in the rewritten result.
type OrderKey struct {
	Column int
	Desc   bool
}

// Limit is the final LIMIT/OFFSET after merge; Count < 0 means no limit.
type Limit struct {
	Offset int64
	Count  int64
}

// AggregateFunc is a mergeable aggregate.
type AggregateFunc uint8

const (
	AggCount AggregateFunc = iota
	AggSum
	AggMin
	AggMax
	AggAvg
)

// AggregateSpec describes one aggregate output column.
type AggregateSpec struct {
	Func     AggregateFunc
	Column   int
	Output   int
	Distinct bool
}

// MergeSpec is the pure merge description for MultiShard plans. Column indexes
// refer to the rewritten per-shard result, including hidden columns.
type MergeSpec struct {
	OrderBy    []OrderKey
	Limit      *Limit
	Aggregates []AggregateSpec
	GroupBy    []int
	Distinct   bool
	Output     []int
}

// Plan is immutable after Build.
type Plan struct {
	Kind    Kind
	Targets []route.Target
	Merge   MergeSpec
	Tx      TxPolicy
	Reject  *errs.MySQL
}

// Builder composes a Router (and later a Rewriter) into a Plan.
type Builder struct {
	Router route.Router
	// Sharding marks the plan as a sharding plan. It is set only by the
	// assembly layer; one target becomes SingleShard + TxSingleShard.
	Sharding bool
}

// Build produces a one-target Passthrough plan for single/rwsplit. In sharding
// mode one target becomes SingleShard + TxSingleShard and several targets
// become MultiShard + TxNone (concat merge, P0 query path).
func (b Builder) Build(ctx context.Context, st *session.State, stmt *sql.Statement) (*Plan, error) {
	if b.Router == nil {
		return nil, fmt.Errorf("plan: nil router")
	}
	if stmt == nil {
		return nil, fmt.Errorf("plan: nil statement")
	}
	if reason := sql.UnsupportedReason(stmt.Raw); reason != "" {
		return &Plan{
			Kind:   Reject,
			Reject: errs.New(errs.ErNotSupportedYet, errs.StateSyntax, reason),
		}, nil
	}
	targets, err := b.Router.Route(ctx, st, stmt)
	if err != nil {
		return nil, err
	}
	switch len(targets) {
	case 0:
		return nil, fmt.Errorf("plan: router returned no targets")
	case 1:
		kind, tx := Passthrough, TxNone
		if b.Sharding {
			kind, tx = SingleShard, TxSingleShard
		}
		return &Plan{Kind: kind, Targets: targets, Tx: tx}, nil
	default:
		if !b.Sharding {
			return &Plan{
				Kind:   Reject,
				Reject: errs.Unknown("multi-target plan not implemented"),
			}, nil
		}
		if reason := multiShardRejectReason(st); reason != "" {
			return &Plan{
				Kind:   Reject,
				Reject: errs.Unsupported("%s", reason),
			}, nil
		}
		return &Plan{Kind: MultiShard, Targets: targets, Tx: TxNone}, nil
	}
}

// multiShardRejectReason lists session states where routing one statement to
// several backends would silently break transaction/connection semantics.
func multiShardRejectReason(st *session.State) string {
	if st == nil {
		return ""
	}
	if !st.Autocommit() {
		return "multi-shard statements are not supported with autocommit=0"
	}
	if st.NeedsSticky() {
		return "multi-shard statements are not supported in a transaction or pinned session"
	}
	return ""
}
