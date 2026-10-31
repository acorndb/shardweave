package sql

import (
	"math"

	"github.com/pingcap/tidb/pkg/parser/ast"
)

// ParseLimit extracts literal LIMIT/OFFSET values. ok=false when the clause is
// absent, not a literal (e.g. a prepared placeholder), negative, or larger
// than int64.
func ParseLimit(l *ast.Limit) (offset, count int64, ok bool) {
	if l == nil || l.Count == nil {
		return 0, 0, false
	}
	count, ok = limitInt(l.Count)
	if !ok {
		return 0, 0, false
	}
	if l.Offset != nil {
		offset, ok = limitInt(l.Offset)
		if !ok {
			return 0, 0, false
		}
	}
	return offset, count, true
}

func limitInt(expr ast.ExprNode) (int64, bool) {
	v, ok := expr.(ast.ValueExpr)
	if !ok {
		return 0, false
	}
	switch n := v.GetValue().(type) {
	case int:
		if n < 0 {
			return 0, false
		}
		return int64(n), true
	case int64:
		if n < 0 {
			return 0, false
		}
		return n, true
	case uint64:
		if n > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	default:
		return 0, false
	}
}
