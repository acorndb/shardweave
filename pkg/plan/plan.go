// Package plan builds execution plans from statements and routing decisions.
package plan

import (
	"context"
	"fmt"

	"github.com/pingcap/tidb/pkg/parser/ast"

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

// OrderKey is one ORDER BY key. Column >= 0 is a 0-based output column index;
// Column < 0 means resolve Name against the result fields (Name or OrgName).
type OrderKey struct {
	Column int
	Name   string
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
		merge, rej := multiShardMerge(stmt)
		if rej != nil {
			return &Plan{Kind: Reject, Reject: rej}, nil
		}
		return &Plan{Kind: MultiShard, Targets: targets, Tx: TxNone, Merge: merge}, nil
	}
}

// multiShardMerge extracts merge hints that need no SQL rewrite. ORDER BY is
// evaluated by each shard and merged with a k-way heap; LIMIT without OFFSET
// is safe after the merge (each shard already applied the same LIMIT).
// OFFSET needs per-shard rewriting and is rejected until D5.1.
func multiShardMerge(stmt *sql.Statement) (MergeSpec, *errs.MySQL) {
	sel, ok := stmt.AST.(*ast.SelectStmt)
	if !ok {
		return MergeSpec{}, nil
	}
	keys, rej := orderKeys(sel)
	if rej != nil {
		return MergeSpec{}, rej
	}
	spec := MergeSpec{OrderBy: keys}
	if sel.Limit == nil {
		return spec, nil
	}
	offset, count, ok := sql.ParseLimit(sel.Limit)
	if !ok {
		return MergeSpec{}, errs.Unsupported("multi-shard LIMIT must be literal integers")
	}
	if offset != 0 {
		return MergeSpec{}, errs.Unsupported("multi-shard LIMIT with OFFSET is not supported")
	}
	spec.Limit = &Limit{Offset: offset, Count: count}
	return spec, nil
}

func orderKeys(sel *ast.SelectStmt) ([]OrderKey, *errs.MySQL) {
	if sel.OrderBy == nil {
		return nil, nil
	}
	keys := make([]OrderKey, 0, len(sel.OrderBy.Items))
	for _, item := range sel.OrderBy.Items {
		switch e := item.Expr.(type) {
		case *ast.ColumnNameExpr:
			keys = append(keys, OrderKey{Column: -1, Name: e.Name.Name.L, Desc: item.Desc})
		case *ast.PositionExpr:
			if e.N < 1 {
				return nil, errs.Unsupported("multi-shard ORDER BY position must be >= 1")
			}
			keys = append(keys, OrderKey{Column: e.N - 1, Desc: item.Desc})
		default:
			return nil, errs.Unsupported("multi-shard ORDER BY supports column names or positions only")
		}
	}
	return keys, nil
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
