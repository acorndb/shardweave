// Package plan builds execution plans from statements and routing decisions.
package plan

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/route"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
	"github.com/acorndb/shardweave/pkg/sql/rewrite"
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

// OrderKeyKind says how an ORDER BY key is addressed.
type OrderKeyKind uint8

const (
	// OrderByPosition uses Column as a 0-based output index (ORDER BY 1).
	OrderByPosition OrderKeyKind = iota
	// OrderByName resolves Name against result Field.Name/OrgName.
	OrderByName
)

// OrderKey is one ORDER BY key.
type OrderKey struct {
	Kind   OrderKeyKind
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
	// CountColumn is the hidden COUNT column for AVG; zero for other funcs.
	CountColumn int
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
		merge, rej := multiShardMerge(stmt, targets)
		if rej != nil {
			return &Plan{Kind: Reject, Reject: rej}, nil
		}
		return &Plan{Kind: MultiShard, Targets: targets, Tx: TxNone, Merge: merge}, nil
	}
}

// multiShardMerge extracts merge hints and rewrites LIMIT when OFFSET is used.
// ORDER BY is evaluated by each shard and merged with a k-way heap. For
// LIMIT m, n each shard receives LIMIT m+n and the merged stream skips m rows.
func multiShardMerge(stmt *sql.Statement, targets []route.Target) (MergeSpec, *errs.MySQL) {
	sel, ok := stmt.AST.(*ast.SelectStmt)
	if !ok {
		return MergeSpec{}, nil
	}
	aggs, extras, rej := aggregateSpecs(sel)
	if rej != nil {
		return MergeSpec{}, rej
	}
	spec := MergeSpec{}
	if len(aggs) > 0 {
		if sel.OrderBy != nil {
			return MergeSpec{}, errs.Unsupported("multi-shard aggregate with ORDER BY is not supported")
		}
		spec.Aggregates = aggs
	} else {
		keys, rej := orderKeys(sel)
		if rej != nil {
			return MergeSpec{}, rej
		}
		spec.OrderBy = keys
	}
	return applyRewrite(sel, targets, spec, extras)
}

// applyRewrite parses LIMIT and rewrites per-shard SQL when OFFSET or hidden
// aggregate columns (AVG's SUM/COUNT) require it.
func applyRewrite(sel *ast.SelectStmt, targets []route.Target, spec MergeSpec, extras []rewrite.ExtraAggregate) (MergeSpec, *errs.MySQL) {
	var perShard *int64
	if sel.Limit != nil {
		offset, count, ok := sql.ParseLimit(sel.Limit)
		if !ok {
			return MergeSpec{}, errs.Unsupported("multi-shard LIMIT must be literal integers")
		}
		if offset > 0 {
			if count > math.MaxInt64-offset {
				return MergeSpec{}, errs.Unsupported("multi-shard LIMIT OFFSET is too large")
			}
			n := offset + count
			perShard = &n
		}
		spec.Limit = &Limit{Offset: offset, Count: count}
	}
	if perShard == nil && len(extras) == 0 {
		return spec, nil
	}
	sqlText, err := rewrite.Select(sel, rewrite.Spec{
		PerShardLimit:   perShard,
		ExtraAggregates: extras,
	})
	if err != nil {
		return MergeSpec{}, errs.Unsupported("multi-shard rewrite failed: %v", err)
	}
	for i := range targets {
		targets[i].SQL = sqlText
	}
	return spec, nil
}

// aggregateSpecs accepts aggregate-only projections without GROUP BY.
// AVG is rewritten to hidden SUM/COUNT columns and merged from those.
func aggregateSpecs(sel *ast.SelectStmt) ([]AggregateSpec, []rewrite.ExtraAggregate, *errs.MySQL) {
	if sel.Fields == nil || len(sel.Fields.Fields) == 0 {
		return nil, nil, nil
	}
	base := len(sel.Fields.Fields)
	avgIdx := 0
	specs := make([]AggregateSpec, 0, base)
	var extras []rewrite.ExtraAggregate
	for i, field := range sel.Fields.Fields {
		agg, ok := field.Expr.(*ast.AggregateFuncExpr)
		if !ok {
			if hasAggregate(field.Expr) {
				return nil, nil, errs.Unsupported("multi-shard aggregate must be a top-level aggregate field")
			}
			if len(specs) > 0 {
				return nil, nil, errs.Unsupported("multi-shard aggregate query must project only aggregate functions")
			}
			continue
		}
		if agg.Distinct {
			return nil, nil, errs.Unsupported("multi-shard aggregate DISTINCT is not supported")
		}
		fn := AggregateFunc(0)
		switch strings.ToLower(agg.F) {
		case "count":
			fn = AggCount
		case "sum":
			fn = AggSum
		case "min":
			fn = AggMin
		case "max":
			fn = AggMax
		case "avg":
			if len(agg.Args) != 1 {
				return nil, nil, errs.Unsupported("multi-shard AVG requires exactly one argument")
			}
			sumIdx := base + 2*avgIdx
			extras = append(extras,
				rewrite.ExtraAggregate{
					Func:  "sum",
					Expr:  agg.Args[0],
					Alias: fmt.Sprintf("__sw_avg_sum_%d", avgIdx),
				},
				rewrite.ExtraAggregate{
					Func:  "count",
					Expr:  agg.Args[0],
					Alias: fmt.Sprintf("__sw_avg_count_%d", avgIdx),
				},
			)
			avgIdx++
			specs = append(specs, AggregateSpec{
				Func: AggAvg, Column: sumIdx, CountColumn: sumIdx + 1, Output: i,
			})
			continue
		default:
			return nil, nil, errs.Unsupported("multi-shard aggregate %s is not supported", agg.F)
		}
		specs = append(specs, AggregateSpec{Func: fn, Column: i, Output: i})
	}
	if len(specs) > 0 && len(specs) != len(sel.Fields.Fields) {
		return nil, nil, errs.Unsupported("multi-shard aggregate query must project only aggregate functions")
	}
	return specs, extras, nil
}

func orderKeys(sel *ast.SelectStmt) ([]OrderKey, *errs.MySQL) {
	if sel.OrderBy == nil {
		return nil, nil
	}
	keys := make([]OrderKey, 0, len(sel.OrderBy.Items))
	for _, item := range sel.OrderBy.Items {
		switch e := item.Expr.(type) {
		case *ast.ColumnNameExpr:
			keys = append(keys, OrderKey{Kind: OrderByName, Name: e.Name.Name.L, Desc: item.Desc})
		case *ast.PositionExpr:
			if e.N < 1 {
				return nil, errs.Unsupported("multi-shard ORDER BY position must be >= 1")
			}
			keys = append(keys, OrderKey{Kind: OrderByPosition, Column: e.N - 1, Desc: item.Desc})
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

type aggregateFinder struct{ found bool }

func (v *aggregateFinder) Enter(n ast.Node) bool {
	if _, ok := n.(*ast.AggregateFuncExpr); ok {
		v.found = true
	}
	return v.found
}

func (v *aggregateFinder) Leave(ast.Node) bool { return !v.found }

func hasAggregate(expr ast.ExprNode) bool {
	if expr == nil {
		return false
	}
	v := &aggregateFinder{}
	ast.Walk(expr, v)
	return v.found
}
