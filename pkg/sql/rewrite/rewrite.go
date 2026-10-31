// Package rewrite produces per-shard SQL from a parsed statement.
// It only manipulates AST/text and never touches connections or routing.
package rewrite

import (
	"fmt"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/format"
)

// Spec describes the per-shard SQL changes for one statement.
type Spec struct {
	// PerShardLimit, when non-nil, replaces LIMIT with this count. Callers use
	// offset+count for multi-shard LIMIT/OFFSET so each shard returns enough
	// rows for the global merge to skip the offset.
	PerShardLimit *int64
}

// Select restores sel with spec applied. The caller's AST is restored before
// returning, so a statement can be rewritten for multiple targets safely.
func Select(sel *ast.SelectStmt, spec Spec) (string, error) {
	if sel == nil {
		return "", fmt.Errorf("rewrite: nil SELECT")
	}
	old := sel.Limit
	if spec.PerShardLimit != nil {
		sel.Limit = &ast.Limit{Count: ast.NewValueExpr(*spec.PerShardLimit, "", "")}
	}
	defer func() { sel.Limit = old }()

	var sb strings.Builder
	ctx := format.NewRestoreCtx(format.DefaultRestoreFlags, &sb)
	if err := sel.Restore(ctx); err != nil {
		return "", err
	}
	return sb.String(), nil
}
