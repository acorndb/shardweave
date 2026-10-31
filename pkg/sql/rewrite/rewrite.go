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

// Spec describes the per-shard SQL changes for one statement.
type Spec struct {
	// PerShardLimit, when non-nil, replaces LIMIT with this count. Callers use
	// offset+count for multi-shard LIMIT/OFFSET so each shard returns enough
	// rows for the global merge to skip the offset.
	PerShardLimit *int64
	// ExtraAggregates are appended to the SELECT list after the original
	// fields. Hidden-column positions are therefore len(original)+2*i, +1.
	ExtraAggregates []ExtraAggregate
}

// Select restores sel with spec applied. The caller's AST is restored before
// returning, so a statement can be rewritten for multiple targets safely.
func Select(sel *ast.SelectStmt, spec Spec) (string, error) {
	if sel == nil {
		return "", fmt.Errorf("rewrite: nil SELECT")
	}
	oldLimit := sel.Limit
	oldFields := sel.Fields
	if spec.PerShardLimit != nil {
		sel.Limit = &ast.Limit{Count: ast.NewValueExpr(*spec.PerShardLimit, "", "")}
	}
	if len(spec.ExtraAggregates) > 0 {
		original := sel.Fields.Fields
		fields := make([]*ast.SelectField, len(original), len(original)+len(spec.ExtraAggregates))
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
		sel.Fields = &ast.FieldList{Fields: fields}
	}
	defer func() {
		sel.Limit = oldLimit
		sel.Fields = oldFields
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
