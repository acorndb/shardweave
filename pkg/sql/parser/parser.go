package parser

import (
	"fmt"

	tidbparser "github.com/pingcap/tidb/pkg/parser"
	"github.com/pingcap/tidb/pkg/parser/ast"
)

// Result is a successful parse outcome.
type Result struct {
	Stmts    []ast.StmtNode
	Warnings []error
}

// Parse converts SQL text to AST statements.
// Panics from the underlying parser are recovered into error returns.
func Parse(sql string) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("parser panic: %v", r)
			res = Result{}
		}
	}()

	p := tidbparser.New()
	stmts, warns, perr := p.Parse(sql, "", "")
	if perr != nil {
		return Result{}, perr
	}
	return Result{Stmts: stmts, Warnings: warns}, nil
}
