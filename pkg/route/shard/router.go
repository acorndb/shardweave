// Package shard routes statements over static sharding rules.
package shard

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/opcode"

	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/route"
	"github.com/acorndb/shardweave/pkg/route/source"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
)

// Rule maps one logical table to sharded slices.
type Rule struct {
	Table     string
	Key       string
	Algorithm string
	Slices    []string
}

// Router routes single-shard statements over static rules.
type Router struct {
	rules        map[string]Rule
	defaultSlice string
}

// New validates and builds a Router. Rule slices keep their configured order;
// hash_mod indexes into that order, so reordering changes routing.
func New(rules []Rule) (*Router, error) {
	if len(rules) == 0 {
		return nil, fmt.Errorf("shard: at least 1 rule is required")
	}
	m := make(map[string]Rule, len(rules))
	for i, r := range rules {
		if r.Table == "" {
			return nil, fmt.Errorf("shard: rules[%d].table is required", i)
		}
		if r.Key == "" {
			return nil, fmt.Errorf("shard: rule %q key is required", r.Table)
		}
		if r.Algorithm != constant.ShardingHashMod {
			return nil, fmt.Errorf("shard: rule %q algorithm %q unsupported", r.Table, r.Algorithm)
		}
		if len(r.Slices) == 0 {
			return nil, fmt.Errorf("shard: rule %q requires slices", r.Table)
		}
		name := strings.ToLower(r.Table)
		if _, ok := m[name]; ok {
			return nil, fmt.Errorf("shard: duplicate table %q", r.Table)
		}
		r.Slices = append([]string(nil), r.Slices...)
		m[name] = r
	}
	return &Router{rules: m, defaultSlice: rules[0].Slices[0]}, nil
}

// Route implements route.Router. P0 only routes to one slice; multi-shard
// statements are rejected.
func (r *Router) Route(_ context.Context, st *session.State, stmt *sql.Statement) ([]route.Target, error) {
	if stmt == nil {
		return nil, reject("nil statement")
	}
	if stmt.Cmd != mysql.ComQuery {
		return nil, reject("prepared statements are not supported in sharding mode")
	}
	if stmt.AST == nil {
		return nil, reject("cannot route statement without AST")
	}

	var (
		tn     *ast.TableName
		rule   Rule
		values []any
		err    error
	)
	switch s := stmt.AST.(type) {
	case *ast.SelectStmt:
		if s.From == nil {
			// Constant selects and the proxy's rewritten LAST_INSERT_ID()/
			// ROW_COUNT() have no table; any slice can answer the literal.
			return []route.Target{{
				Slice: r.defaultSlice,
				Role:  source.PickRole(st, stmt.Cmd, stmt.Raw),
			}}, nil
		}
		tn, err = singleTable(s.From)
		if err == nil {
			rule, err = r.lookupRule(tn)
		}
		if err == nil {
			values, err = shardKeyValues(s.Where, rule.Key)
		}
	case *ast.UpdateStmt:
		if s.MultipleTable {
			return nil, reject("multi-table UPDATE is not supported")
		}
		tn, err = singleTable(s.TableRefs)
		if err == nil {
			rule, err = r.lookupRule(tn)
		}
		if err == nil {
			err = checkAssignments(s.List, rule.Key)
		}
		if err == nil {
			values, err = shardKeyValues(s.Where, rule.Key)
		}
	case *ast.DeleteStmt:
		if s.IsMultiTable || s.Tables != nil {
			return nil, reject("multi-table DELETE is not supported")
		}
		tn, err = singleTable(s.TableRefs)
		if err == nil {
			rule, err = r.lookupRule(tn)
		}
		if err == nil {
			values, err = shardKeyValues(s.Where, rule.Key)
		}
	case *ast.InsertStmt:
		tn, err = singleTable(s.Table)
		if err == nil {
			rule, err = r.lookupRule(tn)
		}
		if err == nil {
			values, err = insertKeyValues(s, rule.Key)
		}
	default:
		return nil, reject("statement type %T is not supported in sharding mode", stmt.AST)
	}
	if err != nil {
		return nil, err
	}
	slice, err := pickSlice(rule, values)
	if err != nil {
		return nil, err
	}
	return []route.Target{{
		Slice: slice,
		Role:  source.PickRole(st, stmt.Cmd, stmt.Raw),
	}}, nil
}

func (r *Router) lookupRule(tn *ast.TableName) (Rule, error) {
	rule, ok := r.rules[strings.ToLower(tn.Name.L)]
	if !ok {
		return Rule{}, reject("table %q is not sharded", tn.Name.O)
	}
	return rule, nil
}

// singleTable accepts exactly one plain table and rejects joins, derived tables
// and schema-qualified tables.
func singleTable(refs *ast.TableRefsClause) (*ast.TableName, error) {
	if refs == nil || refs.TableRefs == nil {
		return nil, reject("table is required")
	}
	j := refs.TableRefs
	if j.Right != nil {
		return nil, reject("joins/multiple tables are not supported")
	}
	ts, ok := j.Left.(*ast.TableSource)
	if !ok {
		return nil, reject("derived/joined tables are not supported")
	}
	tn, ok := ts.Source.(*ast.TableName)
	if !ok {
		return nil, reject("derived tables are not supported")
	}
	if tn.Schema.L != "" {
		return nil, reject("schema-qualified tables are not supported")
	}
	return tn, nil
}

func checkAssignments(list []*ast.Assignment, key string) error {
	for _, a := range list {
		if a.Column != nil && strings.EqualFold(a.Column.Name.L, key) {
			return reject("updating the shard key is not supported")
		}
	}
	return nil
}

func insertKeyValues(s *ast.InsertStmt, key string) ([]any, error) {
	if s.Select != nil {
		return nil, reject("INSERT ... SELECT is not supported")
	}
	if s.Setlist || len(s.Columns) == 0 {
		return nil, reject("INSERT must list the shard key column")
	}
	for _, a := range s.OnDuplicate {
		if a.Column != nil && strings.EqualFold(a.Column.Name.L, key) {
			return nil, reject("ON DUPLICATE KEY UPDATE of the shard key is not supported")
		}
	}
	idx := -1
	for i, c := range s.Columns {
		if strings.EqualFold(c.Name.L, key) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, reject("INSERT must include shard key %q", key)
	}
	values := make([]any, 0, len(s.Lists))
	for _, row := range s.Lists {
		if idx >= len(row) {
			return nil, reject("INSERT row is missing shard key %q", key)
		}
		v, ok := literalValue(row[idx])
		if !ok {
			return nil, reject("shard key must be a literal")
		}
		values = append(values, v)
	}
	return values, nil
}

func shardKeyValues(expr ast.ExprNode, key string) ([]any, error) {
	if key == "" {
		return nil, reject("shard key is not configured")
	}
	values, complete := keyValues(expr, key)
	if !complete || len(values) == 0 {
		return nil, reject("WHERE must constrain shard key %q with = or IN literals", key)
	}
	return values, nil
}

// keyValues finds shard-key literal constraints. complete=false means the
// expression can match rows outside the returned values, so routing is unsafe.
func keyValues(expr ast.ExprNode, key string) (values []any, complete bool) {
	switch e := expr.(type) {
	case *ast.ParenthesesExpr:
		return keyValues(e.Expr, key)
	case *ast.BinaryOperationExpr:
		switch e.Op {
		case opcode.EQ, opcode.NullEQ:
			if isKeyColumn(e.L, key) {
				if v, ok := literalValue(e.R); ok {
					return []any{v}, true
				}
			}
			if isKeyColumn(e.R, key) {
				if v, ok := literalValue(e.L); ok {
					return []any{v}, true
				}
			}
			return nil, false
		case opcode.LogicAnd:
			lv, lok := keyValues(e.L, key)
			rv, rok := keyValues(e.R, key)
			switch {
			case lok && rok:
				return append(lv, rv...), true
			case lok:
				return lv, true
			case rok:
				return rv, true
			default:
				return nil, false
			}
		case opcode.LogicOr:
			lv, lok := keyValues(e.L, key)
			rv, rok := keyValues(e.R, key)
			if !lok || !rok {
				return nil, false
			}
			return append(lv, rv...), true
		default:
			return nil, false
		}
	case *ast.PatternInExpr:
		if e.Not || e.Sel != nil || !isKeyColumn(e.Expr, key) {
			return nil, false
		}
		for _, item := range e.List {
			v, ok := literalValue(item)
			if !ok {
				return nil, false
			}
			values = append(values, v)
		}
		return values, len(values) > 0
	default:
		return nil, false
	}
}

func isKeyColumn(expr ast.ExprNode, key string) bool {
	c, ok := expr.(*ast.ColumnNameExpr)
	if !ok {
		return false
	}
	return strings.EqualFold(c.Name.Name.L, key)
}

func literalValue(expr ast.ExprNode) (any, bool) {
	v, ok := expr.(ast.ValueExpr)
	if !ok {
		return nil, false
	}
	val := v.GetValue()
	return val, val != nil
}

func pickSlice(rule Rule, values []any) (string, error) {
	if len(values) == 0 {
		return "", reject("no shard key value")
	}
	var chosen string
	for _, v := range values {
		idx, err := shardIndex(v, len(rule.Slices))
		if err != nil {
			return "", err
		}
		s := rule.Slices[idx]
		if chosen == "" {
			chosen = s
			continue
		}
		if chosen != s {
			return "", reject("statement spans multiple shards")
		}
	}
	return chosen, nil
}

func shardIndex(value any, n int) (int, error) {
	if n <= 0 {
		return 0, reject("sharding rule has no slices")
	}
	switch v := value.(type) {
	case int:
		return int(uint64(v) % uint64(n)), nil
	case int64:
		return int(uint64(v) % uint64(n)), nil
	case uint64:
		return int(v % uint64(n)), nil
	case string:
		h := fnv.New64a()
		_, _ = h.Write([]byte(v))
		return int(h.Sum64() % uint64(n)), nil
	case []byte:
		h := fnv.New64a()
		_, _ = h.Write(v)
		return int(h.Sum64() % uint64(n)), nil
	default:
		return 0, reject("unsupported shard key type %T", value)
	}
}

func reject(format string, args ...any) error {
	return &plan.RejectError{
		Code:    1235,
		State:   "42000",
		Message: fmt.Sprintf(format, args...),
	}
}
