// Package plan builds execution plans from statements and routing decisions.
package plan

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/format"
	"github.com/pingcap/tidb/pkg/parser/opcode"

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

// HavingOp is one HAVING comparison operator.
type HavingOp uint8

const (
	HavingEQ HavingOp = iota
	HavingNE
	HavingLT
	HavingLE
	HavingGT
	HavingGE
)

// HavingPredicate is one comparison against a grouped output column. Values are
// canonical numeric strings; HAVING is AND-only for now.
type HavingPredicate struct {
	Column int
	Op     HavingOp
	Value  string
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
	Func   AggregateFunc
	Column int
	Output int
	// Distinct means Column points at a hidden raw argument column; the merge
	// layer deduplicates values across shards before applying Func.
	Distinct bool
	// CountColumn is the hidden COUNT column for AVG; zero for other funcs.
	CountColumn int
}

// distinctArg is one DISTINCT aggregate argument projected as a hidden raw
// column and appended to the per-shard GROUP BY.
type distinctArg struct {
	specIndex int
	arg       ast.ExprNode
	alias     string
}

// MergeSpec is the pure merge description for MultiShard plans. Column indexes
// refer to the rewritten per-shard result, including hidden columns.
type MergeSpec struct {
	OrderBy    []OrderKey
	Having     []HavingPredicate
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
	if sel.GroupBy != nil && sel.GroupBy.Rollup {
		return MergeSpec{}, errs.Unsupported("multi-shard GROUP BY WITH ROLLUP is not supported")
	}
	if sel.Having != nil && sel.GroupBy == nil {
		return MergeSpec{}, errs.Unsupported("HAVING requires GROUP BY in sharding mode")
	}
	if sel.Distinct {
		if sel.GroupBy != nil {
			return MergeSpec{}, errs.Unsupported("multi-shard DISTINCT with GROUP BY is not supported")
		}
		for _, field := range sel.Fields.Fields {
			if field.Expr != nil && hasAggregate(field.Expr) {
				return MergeSpec{}, errs.Unsupported("multi-shard DISTINCT with aggregates is not supported")
			}
		}
		keys, rej := postOrderKeys(sel)
		if rej != nil {
			return MergeSpec{}, rej
		}
		spec := MergeSpec{Distinct: true, OrderBy: keys}
		removeLimit := false
		if sel.Limit != nil {
			offset, count, ok := sql.ParseLimit(sel.Limit)
			if !ok {
				return MergeSpec{}, errs.Unsupported("multi-shard LIMIT must be literal integers")
			}
			spec.Limit = &Limit{Offset: offset, Count: count}
			removeLimit = true
		}
		return applyRewrite(sel, targets, spec, nil, nil, nil, removeLimit, false)
	}
	if sel.GroupBy != nil {
		spec, extras, extraFields, groupByAliases, rej := groupSpecs(sel)
		if rej != nil {
			return MergeSpec{}, rej
		}
		keys, rej := postOrderKeys(sel)
		if rej != nil {
			return MergeSpec{}, rej
		}
		spec.OrderBy = keys
		having, rej := havingPredicates(sel)
		if rej != nil {
			return MergeSpec{}, rej
		}
		spec.Having = having
		removeLimit := false
		if sel.Limit != nil {
			offset, count, ok := sql.ParseLimit(sel.Limit)
			if !ok {
				return MergeSpec{}, errs.Unsupported("multi-shard LIMIT must be literal integers")
			}
			spec.Limit = &Limit{Offset: offset, Count: count}
			removeLimit = true
		}
		return applyRewrite(sel, targets, spec, extras, extraFields, groupByAliases, removeLimit, len(having) > 0)
	}
	aggs, extras, extraFields, groupByAliases, rej := aggregateSpecs(sel)
	if rej != nil {
		return MergeSpec{}, rej
	}
	spec := MergeSpec{}
	if len(aggs) > 0 {
		if sel.OrderBy != nil {
			return MergeSpec{}, errs.Unsupported("multi-shard aggregate with ORDER BY is not supported")
		}
		spec.Aggregates = aggs
		removeLimit := len(groupByAliases) > 0
		if removeLimit && sel.Limit != nil {
			offset, count, ok := sql.ParseLimit(sel.Limit)
			if !ok {
				return MergeSpec{}, errs.Unsupported("multi-shard LIMIT must be literal integers")
			}
			spec.Limit = &Limit{Offset: offset, Count: count}
		}
		return applyRewrite(sel, targets, spec, extras, extraFields, groupByAliases, removeLimit, false)
	}
	keys, orderFields, output, rej := orderSpec(sel)
	if rej != nil {
		return MergeSpec{}, rej
	}
	spec.OrderBy = keys
	spec.Output = output
	return applyRewrite(sel, targets, spec, nil, orderFields, nil, false, false)
}

// applyRewrite parses LIMIT and rewrites per-shard SQL when OFFSET or hidden
// aggregate columns (AVG's SUM/COUNT) require it.
func applyRewrite(
	sel *ast.SelectStmt,
	targets []route.Target,
	spec MergeSpec,
	extras []rewrite.ExtraAggregate,
	orderFields []rewrite.ExtraField,
	groupByAliases []string,
	removeLimit bool,
	removeHaving bool,
) (MergeSpec, *errs.MySQL) {
	var perShard *int64
	if sel.Limit != nil && !removeLimit {
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
	if perShard == nil && len(extras) == 0 && len(orderFields) == 0 && len(groupByAliases) == 0 && !removeLimit && !removeHaving {
		return spec, nil
	}
	sqlText, err := rewrite.Select(sel, rewrite.Spec{
		PerShardLimit:   perShard,
		ExtraAggregates: extras,
		ExtraFields:     orderFields,
		RemoveLimit:     removeLimit,
		RemoveHaving:    removeHaving,
		ExtraGroupBy:    groupByAliases,
	})
	if err != nil {
		return MergeSpec{}, errs.Unsupported("multi-shard rewrite failed: %v", err)
	}
	for i := range targets {
		targets[i].SQL = sqlText
	}
	return spec, nil
}

// havingPredicates parses HAVING into AND-only comparisons on grouped output
// columns (group columns, aggregate aliases or matching aggregate expressions).
func havingPredicates(sel *ast.SelectStmt) ([]HavingPredicate, *errs.MySQL) {
	if sel.Having == nil || sel.Having.Expr == nil {
		return nil, nil
	}
	if sel.GroupBy == nil {
		return nil, errs.Unsupported("HAVING requires GROUP BY in sharding mode")
	}
	var preds []HavingPredicate
	if rej := collectHaving(sel, sel.Having.Expr, &preds); rej != nil {
		return nil, rej
	}
	if len(preds) == 0 {
		return nil, errs.Unsupported("HAVING must contain at least one comparison")
	}
	return preds, nil
}

func collectHaving(sel *ast.SelectStmt, expr ast.ExprNode, out *[]HavingPredicate) *errs.MySQL {
	switch e := expr.(type) {
	case *ast.ParenthesesExpr:
		return collectHaving(sel, e.Expr, out)
	case *ast.BinaryOperationExpr:
		if e.Op == opcode.LogicAnd {
			if rej := collectHaving(sel, e.L, out); rej != nil {
				return rej
			}
			return collectHaving(sel, e.R, out)
		}
		pred, rej := havingComparison(sel, e)
		if rej != nil {
			return rej
		}
		*out = append(*out, *pred)
		return nil
	default:
		return errs.Unsupported("HAVING supports AND-ed comparisons only")
	}
}

func havingComparison(sel *ast.SelectStmt, e *ast.BinaryOperationExpr) (*HavingPredicate, *errs.MySQL) {
	op, ok := havingOp(e.Op)
	if !ok {
		return nil, errs.Unsupported("HAVING operator is not supported")
	}
	col, ok := havingColumn(sel, e.L)
	if ok {
		lit, ok := havingLiteral(e.R)
		if !ok {
			return nil, errs.Unsupported("HAVING right side must be a numeric literal or selected aggregate")
		}
		return &HavingPredicate{Column: col, Op: op, Value: lit}, nil
	}
	col, ok = havingColumn(sel, e.R)
	if !ok {
		return nil, errs.Unsupported("HAVING must compare a selected column/aggregate with a literal")
	}
	lit, ok := havingLiteral(e.L)
	if !ok {
		return nil, errs.Unsupported("HAVING left side must be a numeric literal or selected aggregate")
	}
	return &HavingPredicate{Column: col, Op: flipHavingOp(op), Value: lit}, nil
}

func havingColumn(sel *ast.SelectStmt, expr ast.ExprNode) (int, bool) {
	switch e := expr.(type) {
	case *ast.ColumnNameExpr:
		idx := selectFieldIndex(sel, e.Name.Name.L)
		return idx, idx >= 0
	case *ast.PositionExpr:
		if e.N >= 1 && e.N <= len(sel.Fields.Fields) {
			return e.N - 1, true
		}
	case *ast.AggregateFuncExpr:
		for i, field := range sel.Fields.Fields {
			agg, ok := field.Expr.(*ast.AggregateFuncExpr)
			if ok && sameAggregate(agg, e) {
				return i, true
			}
		}
	}
	return 0, false
}

func sameAggregate(a, b *ast.AggregateFuncExpr) bool {
	if a == nil || b == nil || !strings.EqualFold(a.F, b.F) || a.Distinct != b.Distinct ||
		len(a.Args) != len(b.Args) {
		return false
	}
	for i := range a.Args {
		if exprText(a.Args[i]) != exprText(b.Args[i]) {
			return false
		}
	}
	return true
}

func exprText(expr ast.ExprNode) string {
	var b strings.Builder
	ctx := format.NewRestoreCtx(format.DefaultRestoreFlags, &b)
	if err := expr.Restore(ctx); err != nil {
		return ""
	}
	return b.String()
}

func havingLiteral(expr ast.ExprNode) (string, bool) {
	v, ok := expr.(ast.ValueExpr)
	if !ok {
		return "", false
	}
	switch x := v.GetValue().(type) {
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case uint64:
		return strconv.FormatUint(x, 10), true
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), true
	default:
		return "", false
	}
}

func havingOp(op opcode.Op) (HavingOp, bool) {
	switch op {
	case opcode.EQ:
		return HavingEQ, true
	case opcode.NE:
		return HavingNE, true
	case opcode.LT:
		return HavingLT, true
	case opcode.LE:
		return HavingLE, true
	case opcode.GT:
		return HavingGT, true
	case opcode.GE:
		return HavingGE, true
	default:
		return 0, false
	}
}

func flipHavingOp(op HavingOp) HavingOp {
	switch op {
	case HavingLT:
		return HavingGT
	case HavingLE:
		return HavingGE
	case HavingGT:
		return HavingLT
	case HavingGE:
		return HavingLE
	default:
		return op
	}
}

// groupSpecs accepts projected group columns plus aggregate-only other fields.
// Group keys must be in the select list so the merge layer has their values.
// DISTINCT COUNT/SUM/AVG arguments are appended as hidden raw columns and to
// the per-shard GROUP BY; the merge layer deduplicates them across shards.
func groupSpecs(sel *ast.SelectStmt) (MergeSpec, []rewrite.ExtraAggregate, []rewrite.ExtraField, []string, *errs.MySQL) {
	base := len(sel.Fields.Fields)
	output := make([]int, base)
	for i := range output {
		output[i] = i
	}
	groupSet := make(map[int]bool, len(sel.GroupBy.Items))
	groupBy := make([]int, 0, len(sel.GroupBy.Items))
	addGroup := func(idx int) {
		if !groupSet[idx] {
			groupSet[idx] = true
			groupBy = append(groupBy, idx)
		}
	}
	for _, item := range sel.GroupBy.Items {
		switch e := item.Expr.(type) {
		case *ast.ColumnNameExpr:
			idx := selectFieldIndex(sel, e.Name.Name.L)
			if idx < 0 {
				return MergeSpec{}, nil, nil, nil, errs.Unsupported("GROUP BY column %q must be selected", e.Name.Name.O)
			}
			addGroup(idx)
		case *ast.PositionExpr:
			if e.N < 1 || e.N > base {
				return MergeSpec{}, nil, nil, nil, errs.Unsupported("GROUP BY position %d is out of range", e.N)
			}
			addGroup(e.N - 1)
		default:
			return MergeSpec{}, nil, nil, nil, errs.Unsupported("multi-shard GROUP BY supports columns or positions only")
		}
	}
	if len(groupBy) == 0 {
		return MergeSpec{}, nil, nil, nil, errs.Unsupported("GROUP BY requires at least one column")
	}
	for _, idx := range groupBy {
		if hasAggregate(sel.Fields.Fields[idx].Expr) {
			return MergeSpec{}, nil, nil, nil, errs.Unsupported("GROUP BY cannot reference an aggregate field")
		}
	}

	var distinctArgs []distinctArg
	avgIdx := 0
	specs := make([]AggregateSpec, 0, base-len(groupBy))
	var extras []rewrite.ExtraAggregate
	for i, field := range sel.Fields.Fields {
		if groupSet[i] {
			continue
		}
		agg, ok := field.Expr.(*ast.AggregateFuncExpr)
		if !ok {
			if hasAggregate(field.Expr) {
				return MergeSpec{}, nil, nil, nil, errs.Unsupported("multi-shard aggregate must be a top-level aggregate field")
			}
			return MergeSpec{}, nil, nil, nil, errs.Unsupported("multi-shard GROUP BY fields must be group columns or aggregate functions")
		}
		fn := strings.ToLower(agg.F)
		if agg.Distinct {
			switch fn {
			case "min", "max":
				// DISTINCT does not change MIN/MAX; merge partial aggregates as usual.
			case "count", "sum", "avg":
				if len(agg.Args) != 1 {
					return MergeSpec{}, nil, nil, nil, errs.Unsupported("multi-shard aggregate DISTINCT requires exactly one argument")
				}
				alias := fmt.Sprintf("__sw_distinct_%d", len(distinctArgs))
				distinctArgs = append(distinctArgs, distinctArg{specIndex: len(specs), arg: agg.Args[0], alias: alias})
				specs = append(specs, AggregateSpec{Func: aggregateFunc(fn), Output: i, Distinct: true})
				continue
			default:
				return MergeSpec{}, nil, nil, nil, errs.Unsupported("multi-shard aggregate %s is not supported", agg.F)
			}
		}
		switch fn {
		case "count":
			specs = append(specs, AggregateSpec{Func: AggCount, Column: i, Output: i})
		case "sum":
			specs = append(specs, AggregateSpec{Func: AggSum, Column: i, Output: i})
		case "min":
			specs = append(specs, AggregateSpec{Func: AggMin, Column: i, Output: i})
		case "max":
			specs = append(specs, AggregateSpec{Func: AggMax, Column: i, Output: i})
		case "avg":
			if len(agg.Args) != 1 {
				return MergeSpec{}, nil, nil, nil, errs.Unsupported("multi-shard AVG requires exactly one argument")
			}
			sumIdx := base + 2*avgIdx
			extras = append(extras,
				rewrite.ExtraAggregate{Func: "sum", Expr: agg.Args[0], Alias: fmt.Sprintf("__sw_avg_sum_%d", avgIdx)},
				rewrite.ExtraAggregate{Func: "count", Expr: agg.Args[0], Alias: fmt.Sprintf("__sw_avg_count_%d", avgIdx)},
			)
			avgIdx++
			specs = append(specs, AggregateSpec{Func: AggAvg, Column: sumIdx, CountColumn: sumIdx + 1, Output: i})
		default:
			return MergeSpec{}, nil, nil, nil, errs.Unsupported("multi-shard aggregate %s is not supported", agg.F)
		}
	}
	extraFields, groupByAliases := distinctExtras(&specs, distinctArgs, base+len(extras))
	return MergeSpec{GroupBy: groupBy, Aggregates: specs, Output: output}, extras, extraFields, groupByAliases, nil
}

// distinctExtras assigns hidden raw-value columns to DISTINCT aggregates.
// Hidden aggregates (AVG SUM/COUNT) come first, then one ExtraField per
// DISTINCT argument; per-shard GROUP BY references those field aliases.
func distinctExtras(specs *[]AggregateSpec, args []distinctArg, hiddenBase int) ([]rewrite.ExtraField, []string) {
	fields := make([]rewrite.ExtraField, 0, len(args))
	aliases := make([]string, 0, len(args))
	for j, d := range args {
		(*specs)[d.specIndex].Column = hiddenBase + j
		fields = append(fields, rewrite.ExtraField{Expr: d.arg, Alias: d.alias})
		aliases = append(aliases, d.alias)
	}
	return fields, aliases
}

// aggregateFunc maps a validated aggregate name to its merge function. Callers
// reject unknown names before calling it.
func aggregateFunc(name string) AggregateFunc {
	switch name {
	case "count":
		return AggCount
	case "sum":
		return AggSum
	case "min":
		return AggMin
	case "max":
		return AggMax
	case "avg":
		return AggAvg
	default:
		return AggCount
	}
}

// aggregateSpecs accepts aggregate-only projections without GROUP BY.
// AVG is rewritten to hidden SUM/COUNT columns and merged from those; DISTINCT
// COUNT/SUM/AVG arguments become hidden raw columns plus a per-shard GROUP BY.
func aggregateSpecs(sel *ast.SelectStmt) ([]AggregateSpec, []rewrite.ExtraAggregate, []rewrite.ExtraField, []string, *errs.MySQL) {
	if sel.Fields == nil || len(sel.Fields.Fields) == 0 {
		return nil, nil, nil, nil, nil
	}
	base := len(sel.Fields.Fields)
	avgIdx := 0
	specs := make([]AggregateSpec, 0, base)
	var extras []rewrite.ExtraAggregate
	var distinctArgs []distinctArg
	for i, field := range sel.Fields.Fields {
		agg, ok := field.Expr.(*ast.AggregateFuncExpr)
		if !ok {
			if hasAggregate(field.Expr) {
				return nil, nil, nil, nil, errs.Unsupported("multi-shard aggregate must be a top-level aggregate field")
			}
			if len(specs) > 0 {
				return nil, nil, nil, nil, errs.Unsupported("multi-shard aggregate query must project only aggregate functions")
			}
			continue
		}
		fn := strings.ToLower(agg.F)
		if agg.Distinct {
			switch fn {
			case "min", "max":
				// DISTINCT does not change MIN/MAX; merge partial aggregates as usual.
			case "count", "sum", "avg":
				if len(agg.Args) != 1 {
					return nil, nil, nil, nil, errs.Unsupported("multi-shard aggregate DISTINCT requires exactly one argument")
				}
				alias := fmt.Sprintf("__sw_distinct_%d", len(distinctArgs))
				distinctArgs = append(distinctArgs, distinctArg{specIndex: len(specs), arg: agg.Args[0], alias: alias})
				specs = append(specs, AggregateSpec{Func: aggregateFunc(fn), Output: i, Distinct: true})
				continue
			default:
				return nil, nil, nil, nil, errs.Unsupported("multi-shard aggregate %s is not supported", agg.F)
			}
		}
		switch fn {
		case "count":
			specs = append(specs, AggregateSpec{Func: AggCount, Column: i, Output: i})
		case "sum":
			specs = append(specs, AggregateSpec{Func: AggSum, Column: i, Output: i})
		case "min":
			specs = append(specs, AggregateSpec{Func: AggMin, Column: i, Output: i})
		case "max":
			specs = append(specs, AggregateSpec{Func: AggMax, Column: i, Output: i})
		case "avg":
			if len(agg.Args) != 1 {
				return nil, nil, nil, nil, errs.Unsupported("multi-shard AVG requires exactly one argument")
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
			return nil, nil, nil, nil, errs.Unsupported("multi-shard aggregate %s is not supported", agg.F)
		}
	}
	if len(specs) > 0 && len(specs) != len(sel.Fields.Fields) {
		return nil, nil, nil, nil, errs.Unsupported("multi-shard aggregate query must project only aggregate functions")
	}
	extraFields, groupByAliases := distinctExtras(&specs, distinctArgs, base+len(extras))
	return specs, extras, extraFields, groupByAliases, nil
}

// postOrderKeys resolves ORDER BY against output columns after GROUP BY or
// DISTINCT. Expressions must be selected columns/aliases or positions.
func postOrderKeys(sel *ast.SelectStmt) ([]OrderKey, *errs.MySQL) {
	if sel.OrderBy == nil {
		return nil, nil
	}
	base := len(sel.Fields.Fields)
	keys := make([]OrderKey, 0, len(sel.OrderBy.Items))
	for _, item := range sel.OrderBy.Items {
		switch e := item.Expr.(type) {
		case *ast.PositionExpr:
			if e.N < 1 || e.N > base {
				return nil, errs.Unsupported("ORDER BY position %d is out of range", e.N)
			}
			keys = append(keys, OrderKey{Kind: OrderByPosition, Column: e.N - 1, Desc: item.Desc})
		case *ast.ColumnNameExpr:
			idx := selectFieldIndex(sel, e.Name.Name.L)
			if idx < 0 {
				return nil, errs.Unsupported("ORDER BY column %q must be selected", e.Name.Name.O)
			}
			keys = append(keys, OrderKey{Kind: OrderByPosition, Column: idx, Desc: item.Desc})
		default:
			return nil, errs.Unsupported("GROUP BY/DISTINCT ORDER BY supports selected columns or positions only")
		}
	}
	return keys, nil
}

func orderSpec(sel *ast.SelectStmt) ([]OrderKey, []rewrite.ExtraField, []int, *errs.MySQL) {
	if sel.OrderBy == nil {
		return nil, nil, nil, nil
	}
	base := len(sel.Fields.Fields)
	hasWildcard := false
	for _, field := range sel.Fields.Fields {
		if field.WildCard != nil {
			hasWildcard = true
			break
		}
	}
	var keys []OrderKey
	var extras []rewrite.ExtraField
	var output []int
	for _, item := range sel.OrderBy.Items {
		switch e := item.Expr.(type) {
		case *ast.PositionExpr:
			if e.N < 1 || e.N > base {
				return nil, nil, nil, errs.Unsupported("multi-shard ORDER BY position %d is out of range", e.N)
			}
			keys = append(keys, OrderKey{Kind: OrderByPosition, Column: e.N - 1, Desc: item.Desc})
		case *ast.ColumnNameExpr:
			if hasWildcard {
				keys = append(keys, OrderKey{Kind: OrderByName, Name: e.Name.Name.L, Desc: item.Desc})
				continue
			}
			if idx := selectFieldIndex(sel, e.Name.Name.L); idx >= 0 {
				keys = append(keys, OrderKey{Kind: OrderByPosition, Column: idx, Desc: item.Desc})
				continue
			}
			if output == nil {
				output = make([]int, base)
				for i := range output {
					output[i] = i
				}
			}
			alias := fmt.Sprintf("__sw_order_%d", len(extras))
			extras = append(extras, rewrite.ExtraField{Expr: e, Alias: alias})
			keys = append(keys, OrderKey{
				Kind: OrderByPosition, Column: base + len(extras) - 1, Desc: item.Desc,
			})
		default:
			if hasWildcard {
				return nil, nil, nil, errs.Unsupported("multi-shard ORDER BY expression requires an explicit select list")
			}
			if output == nil {
				output = make([]int, base)
				for i := range output {
					output[i] = i
				}
			}
			alias := fmt.Sprintf("__sw_order_%d", len(extras))
			extras = append(extras, rewrite.ExtraField{Expr: item.Expr, Alias: alias})
			keys = append(keys, OrderKey{
				Kind: OrderByPosition, Column: base + len(extras) - 1, Desc: item.Desc,
			})
		}
	}
	return keys, extras, output, nil
}

func selectFieldIndex(sel *ast.SelectStmt, name string) int {
	for i, field := range sel.Fields.Fields {
		if field.WildCard != nil || field.Expr == nil {
			continue
		}
		if field.AsName.L == name {
			return i
		}
		if c, ok := field.Expr.(*ast.ColumnNameExpr); ok && strings.EqualFold(c.Name.Name.L, name) {
			return i
		}
	}
	return -1
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
