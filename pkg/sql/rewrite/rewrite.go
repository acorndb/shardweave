// Package rewrite produces per-shard SQL from a parsed statement.
// It only manipulates AST/text and never touches connections or routing.
package rewrite

import (
	"fmt"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/format"
)

// ExtraAggregate appends one aggregate column to the per-shard projection.
// AVG uses it to bring back SUM/COUNT hidden columns.
type ExtraAggregate struct {
	Func  string // sum | count
	Expr  ast.ExprNode
	Alias string
}

// ExtraField appends a plain hidden projection column (ORDER BY keys that are
// not selected).
type ExtraField struct {
	Expr  ast.ExprNode
	Alias string
}

// Spec describes the per-shard SQL changes for one statement.
type Spec struct {
	// PerShardLimit, when non-nil, replaces LIMIT with this count. Callers use
	// offset+count for multi-shard LIMIT/OFFSET so each shard returns enough
	// rows for the global merge to skip the offset.
	PerShardLimit *int64
	// ExtraAggregates are appended first, then ExtraFields. Hidden-column
	// positions are therefore len(original)+2*i, +1 for AVG.
	ExtraAggregates []ExtraAggregate
	ExtraFields     []ExtraField
	// RemoveLimit drops LIMIT from the per-shard SQL. GROUP BY/DISTINCT apply
	// LIMIT after the global merge, so per-shard truncation is unsafe.
	RemoveLimit bool
	// RemoveHaving drops HAVING from the per-shard SQL. HAVING on aggregate
	// outputs must be evaluated after cross-shard groups are merged.
	RemoveHaving bool
	// ExtraGroupBy appends GROUP BY references to select aliases. DISTINCT
	// aggregates append their raw argument as an ExtraField and group by that
	// alias, so each distinct value is returned once per shard/group.
	ExtraGroupBy []string
}

// Select restores sel with spec applied. The caller's AST is restored before
// returning, so a statement can be rewritten for multiple targets safely.
func Select(sel *ast.SelectStmt, spec Spec) (string, error) {
	if sel == nil {
		return "", fmt.Errorf("rewrite: nil SELECT")
	}
	for _, alias := range spec.ExtraGroupBy {
		if alias == "" {
			return "", fmt.Errorf("rewrite: empty GROUP BY alias")
		}
	}
	oldLimit := sel.Limit
	oldHaving := sel.Having
	oldFields := sel.Fields
	oldGroupBy := sel.GroupBy
	switch {
	case spec.RemoveLimit:
		sel.Limit = nil
	case spec.PerShardLimit != nil:
		sel.Limit = &ast.Limit{Count: ast.NewValueExpr(*spec.PerShardLimit, "", "")}
	}
	if spec.RemoveHaving {
		sel.Having = nil
	}
	if sel.Fields != nil {
		original := sel.Fields.Fields
		capacity := len(original) + len(spec.ExtraAggregates) + len(spec.ExtraFields)
		fields := make([]*ast.SelectField, len(original), capacity)
		for i, field := range original {
			fields[i] = preserveLabel(field)
		}
		for _, extra := range spec.ExtraAggregates {
			field := &ast.SelectField{Expr: &ast.AggregateFuncExpr{
				F:    extra.Func,
				Args: []ast.ExprNode{extra.Expr},
			}}
			if extra.Alias != "" {
				field.AsName = ast.NewCIStr(extra.Alias)
			}
			fields = append(fields, field)
		}
		for _, extra := range spec.ExtraFields {
			field := &ast.SelectField{Expr: extra.Expr}
			if extra.Alias != "" {
				field.AsName = ast.NewCIStr(extra.Alias)
			}
			fields = append(fields, field)
		}
		sel.Fields = &ast.FieldList{Fields: fields}
	}
	if len(spec.ExtraGroupBy) > 0 {
		items := make([]*ast.ByItem, 0, len(spec.ExtraGroupBy)+1)
		if oldGroupBy != nil {
			items = append(items, oldGroupBy.Items...)
		}
		for _, alias := range spec.ExtraGroupBy {
			items = append(items, &ast.ByItem{
				Expr: &ast.ColumnNameExpr{Name: &ast.ColumnName{Name: ast.NewCIStr(alias)}},
			})
		}
		sel.GroupBy = &ast.GroupByClause{Items: items}
	}
	defer func() {
		sel.Limit = oldLimit
		sel.Having = oldHaving
		sel.Fields = oldFields
		sel.GroupBy = oldGroupBy
	}()

	var sb strings.Builder
	ctx := format.NewRestoreCtx(format.DefaultRestoreFlags, &sb)
	if err := sel.Restore(ctx); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// preserveLabel pins the original column label for expression projections.
// AST restore backquotes identifiers, which changes MySQL's derived label for
// expressions (AVG(amount) -> AVG(`amount`)). An explicit alias equal to the
// parser-retained field text restores the original label. Plain column
// references and wildcards are left alone because their labels are column
// names, not expression text.
func preserveLabel(field *ast.SelectField) *ast.SelectField {
	if field == nil || field.WildCard != nil || field.Expr == nil ||
		field.AsName.O != "" || field.Text() == "" {
		return field
	}
	if _, ok := field.Expr.(*ast.ColumnNameExpr); ok {
		return field
	}
	c := *field
	c.AsName = ast.NewCIStr(field.Text())
	return &c
}
