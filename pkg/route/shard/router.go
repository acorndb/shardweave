// Package shard routes statements over static sharding rules.
package shard

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/pingcap/tidb/pkg/parser/ast"
	"github.com/pingcap/tidb/pkg/parser/opcode"

	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/mysql"
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
		if r.Algorithm != constant.ShardingHashMod && r.Algorithm != constant.ShardingHashModStr {
			return nil, fmt.Errorf("shard: rule %q algorithm %q unsupported", r.Table, r.Algorithm)
		}
		if len(r.Slices) == 0 {
			return nil, fmt.Errorf("shard: rule %q requires slices", r.Table)
		}
		seenSlices := make(map[string]bool, len(r.Slices))
		for j, s := range r.Slices {
			if s == "" {
				return nil, fmt.Errorf("shard: rule %q slices[%d] is empty", r.Table, j)
			}
			if seenSlices[s] {
				return nil, fmt.Errorf("shard: rule %q slices[%d] %q is duplicated", r.Table, j, s)
			}
			seenSlices[s] = true
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
	switch stmt.Cmd {
	case mysql.ComInitDB:
		return r.defaultTargets(constant.RoleMaster), nil
	case mysql.ComStmtExecute, mysql.ComStmtReset, mysql.ComStmtClose:
		if st == nil {
			return nil, reject("nil session for prepared statement")
		}
		entry, ok := st.Stmts().Get(stmt.StmtID)
		if !ok {
			return nil, reject("unknown prepared statement %d", stmt.StmtID)
		}
		return []route.Target{{Slice: entry.Slice, Role: constant.RoleMaster}}, nil
	case mysql.ComStmtPrepare:
		if stmt.AST == nil {
			return nil, reject("cannot route prepared statement without AST")
		}
		sel, ok := stmt.AST.(*ast.SelectStmt)
		if !ok {
			return nil, reject("only prepared SELECT is supported")
		}
		if sel.From == nil {
			return nil, reject("prepared constant SELECT is not supported")
		}
	case mysql.ComQuery:
		// handled below
	default:
		return nil, reject("command %d is not supported in sharding mode", stmt.Cmd)
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
	case *ast.UseStmt:
		return r.defaultTargets(constant.RoleMaster), nil
	case *ast.SetStmt:
		if stmt.EnsureClass().SetNames != "" {
			return r.defaultTargets(constant.RoleMaster), nil
		}
		return nil, reject("SET is not supported in sharding mode")
	case *ast.ShowStmt:
		if isSafeShow(s.Tp) {
			return r.defaultTargets(source.PickRole(st, stmt)), nil
		}
		return nil, reject("SHOW type is not supported in sharding mode")
	case *ast.ExplainStmt:
		if isDescribe(stmt) {
			return r.defaultTargets(source.PickRole(st, stmt)), nil
		}
		return nil, reject("EXPLAIN is not supported in sharding mode; use EXPLAIN ROUTE")
	case *ast.SelectStmt:
		if s.From == nil {
			// Constant selects and the proxy's rewritten LAST_INSERT_ID()/
			// ROW_COUNT() have no table; any slice can answer the literal.
			return []route.Target{{
				Slice: r.defaultSlice,
				Role:  source.PickRole(st, stmt),
			}}, nil
		}
		tn, err = singleTable(s.From)
		if err == nil {
			rule, err = r.lookupRule(tn)
		}
		if err != nil {
			return nil, err
		}
		values, complete := keyValues(s.Where, rule.Key)
		slices := rule.Slices
		if complete && len(values) > 0 {
			slices, err = slicesForValues(rule, values)
			if err != nil {
				return nil, err
			}
		}
		// A keyed query can still span shards (cross-slice IN); merge clauses
		// are only safe when the whole statement stays on one shard.
		if len(slices) > 1 {
			if err := checkScatterSelect(s); err != nil {
				return nil, err
			}
		}
		return targetsForSlices(slices, source.PickRole(st, stmt)), nil
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
		Role:  source.PickRole(st, stmt),
	}}, nil
}

func (r *Router) lookupRule(tn *ast.TableName) (Rule, error) {
	rule, ok := r.rules[strings.ToLower(tn.Name.L)]
	if !ok {
		return Rule{}, reject("table %q is not sharded", tn.Name.O)
	}
	return rule, nil
}

func (r *Router) defaultTargets(role constant.Role) []route.Target {
	return []route.Target{{Slice: r.defaultSlice, Role: role}}
}

func targetsForSlices(slices []string, role constant.Role) []route.Target {
	out := make([]route.Target, 0, len(slices))
	for _, s := range slices {
		out = append(out, route.Target{Slice: s, Role: role})
	}
	return out
}

// slicesForValues maps key literals to their shards, de-duplicated in rule
// order so the scatter plan is deterministic.
func slicesForValues(rule Rule, values []any) ([]string, error) {
	seen := make(map[string]bool, len(rule.Slices))
	for _, v := range values {
		idx, err := shardIndex(rule, v)
		if err != nil {
			return nil, err
		}
		seen[rule.Slices[idx]] = true
	}
	out := make([]string, 0, len(seen))
	for _, s := range rule.Slices {
		if seen[s] {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, reject("no shard key value")
	}
	return out, nil
}

// checkScatterSelect bounds the multi-shard path to shapes the merge layer can
// honor. ORDER BY/LIMIT are validated and rewritten by plan; aggregates,
// DISTINCT, window functions, INTO and locking reads are rejected here.
func checkScatterSelect(s *ast.SelectStmt) error {
	switch {
	case len(s.WindowSpecs) > 0:
		return reject("multi-shard SELECT with window functions is not supported")
	case s.SelectIntoOpt != nil:
		return reject("multi-shard SELECT INTO is not supported")
	case s.LockInfo != nil:
		return reject("multi-shard locking reads are not supported")
	case s.SelectStmtOpts != nil && s.SelectStmtOpts.CalcFoundRows:
		return reject("SQL_CALC_FOUND_ROWS is not supported in sharding mode")
	case selectHasWindow(s):
		return reject("multi-shard window function is not supported")
	case selectHasSubquery(s):
		return reject("multi-shard subqueries are not supported")
	}
	return nil
}

type windowFinder struct{ found bool }

func (v *windowFinder) Enter(n ast.Node) bool {
	if _, ok := n.(*ast.WindowFuncExpr); ok {
		v.found = true
	}
	return v.found
}

func (v *windowFinder) Leave(ast.Node) bool { return !v.found }

func selectHasWindow(s *ast.SelectStmt) bool {
	v := &windowFinder{}
	ast.Walk(s, v)
	return v.found
}

type subqueryFinder struct{ found bool }

func (v *subqueryFinder) Enter(n ast.Node) bool {
	if _, ok := n.(*ast.SubqueryExpr); ok {
		v.found = true
	}
	return v.found
}

func (v *subqueryFinder) Leave(ast.Node) bool { return !v.found }

func selectHasSubquery(s *ast.SelectStmt) bool {
	v := &subqueryFinder{}
	ast.Walk(s, v)
	return v.found
}

// isDescribe reports DESCRIBE/DESC (as opposed to EXPLAIN). First comes from
// the statement Class, so the decision stays in the token layer.
func isDescribe(stmt *sql.Statement) bool {
	switch stmt.EnsureClass().First {
	case "DESC", "DESCRIBE":
		return true
	default:
		return false
	}
}

func isSafeShow(tp ast.ShowStmtType) bool {
	switch tp {
	case ast.ShowTables, ast.ShowColumns, ast.ShowCreateTable, ast.ShowIndex,
		ast.ShowVariables, ast.ShowDatabases:
		return true
	default:
		return false
	}
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
	switch e := expr.(type) {
	case ast.ValueExpr:
		val := e.GetValue()
		return val, val != nil
	case *ast.UnaryOperationExpr:
		switch e.Op {
		case opcode.Plus:
			return literalValue(e.V)
		case opcode.Minus:
			v, ok := literalValue(e.V)
			if !ok {
				return nil, false
			}
			switch n := v.(type) {
			case int:
				return -n, true
			case int64:
				return -n, true
			}
		}
	}
	return nil, false
}

func pickSlice(rule Rule, values []any) (string, error) {
	if len(values) == 0 {
		return "", reject("no shard key value")
	}
	var chosen string
	for _, v := range values {
		idx, err := shardIndex(rule, v)
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

// shardIndex canonicalizes the key according to the rule's algorithm. The
// algorithm, not the SQL literal type, decides the routing domain:
// hash_mod is integer-only and hash_mod_str is string-only. Mixed literal
// forms ('1' vs 1) therefore cannot silently land on different slices.
func shardIndex(rule Rule, value any) (int, error) {
	n := len(rule.Slices)
	if n <= 0 {
		return 0, reject("sharding rule has no slices")
	}
	switch rule.Algorithm {
	case constant.ShardingHashMod:
		v, ok := integerKey(value)
		if !ok {
			return 0, reject("shard key %v is not an integer literal for %s", value, rule.Algorithm)
		}
		return int(v % uint64(n)), nil
	case constant.ShardingHashModStr:
		switch v := value.(type) {
		case string:
			return int(fnvSum(v) % uint64(n)), nil
		case []byte:
			return int(fnvSum(v) % uint64(n)), nil
		default:
			return 0, reject("shard key %v is not a string literal for %s", value, rule.Algorithm)
		}
	default:
		return 0, reject("sharding algorithm %q is not supported", rule.Algorithm)
	}
}

// integerKey accepts integer literals and strings that parse as exact base-10
// integers, matching MySQL's numeric coercion for quoted numbers. Decimal,
// hex, bit and non-numeric strings are rejected so they cannot fall back to a
// different hash domain.
func integerKey(value any) (uint64, bool) {
	switch v := value.(type) {
	case int:
		return uint64(v), true
	case int64:
		return uint64(v), true
	case uint64:
		return v, true
	case string:
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return uint64(n), true
		}
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

// fnvSum is FNV-1a 64 over v. The hand-rolled loop avoids the hash/fnv
// interface and, for string keys, the []byte conversion copy.
func fnvSum[T ~string | ~[]byte](v T) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(v); i++ {
		h ^= uint64(v[i])
		h *= prime64
	}
	return h
}

func reject(format string, args ...any) error {
	return errs.Unsupported(format, args...)
}
