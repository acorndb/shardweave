// Package randgen generates valid, seeded MySQL SELECT statements for
// differential testing. It is deliberately dependency-free and covers a
// bounded grammar over a caller-supplied schema: projections, arithmetic,
// aggregates, joins, predicates, ORDER BY and LIMIT.
package randgen

import (
	"fmt"
	"math/rand"
	"strings"
)

// Kind is the coarse column type used to keep generated expressions valid.
type Kind uint8

const (
	Int Kind = iota
	Float
	Text
	Date
)

// Column describes one column the generator may reference.
type Column struct {
	Name string
	Kind Kind
}

// Table describes one table and its columns.
type Table struct {
	Name    string
	Columns []Column
}

// Generator is a deterministic SQL generator. Separate instances with the same
// seed produce the same sequence.
type Generator struct {
	rnd    *rand.Rand
	tables []Table
}

// New returns a generator over tables. It panics on an empty schema, a table
// without columns, or a table without the "id" join key and a numeric column.
// Those are test wiring bugs: the grammar assumes both.
func New(seed int64, tables []Table) *Generator {
	if len(tables) == 0 {
		panic("randgen: empty schema")
	}
	for _, t := range tables {
		if len(t.Columns) == 0 {
			panic("randgen: table without columns: " + t.Name)
		}
		var hasID, hasNumeric bool
		for _, c := range t.Columns {
			hasID = hasID || c.Name == "id"
			hasNumeric = hasNumeric || c.Kind == Int || c.Kind == Float
		}
		if !hasID {
			panic("randgen: table without id column: " + t.Name)
		}
		if !hasNumeric {
			panic("randgen: table without numeric column: " + t.Name)
		}
	}
	return &Generator{rnd: rand.New(rand.NewSource(seed)), tables: tables}
}

// Select returns one random SELECT statement.
func (g *Generator) Select() string {
	t := g.tables[g.rnd.Intn(len(g.tables))]
	if g.rnd.Intn(5) == 0 {
		return g.aggregateSelect(t)
	}
	return g.rowSelect(t)
}

func (g *Generator) rowSelect(t Table) string {
	var b strings.Builder
	b.WriteString("SELECT ")

	n := 1 + g.rnd.Intn(3)
	cols := make([]string, 0, n)
	orderable := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if g.rnd.Intn(4) == 0 {
			cols = append(cols, g.numericExpr(t))
			continue
		}
		col := t.Columns[g.rnd.Intn(len(t.Columns))]
		ref := t.Name + "." + col.Name
		cols = append(cols, ref)
		orderable = append(orderable, ref)
	}
	b.WriteString(strings.Join(cols, ", "))
	b.WriteString(" FROM ")
	b.WriteString(t.Name)

	join := g.joinTable(t)
	if join != nil {
		b.WriteString(" JOIN ")
		b.WriteString(join.Name)
		b.WriteString(" ON ")
		b.WriteString(t.Name + ".id = " + join.Name + ".id")
	}

	if p := g.whereClause(t, join); p != "" {
		b.WriteString(" WHERE ")
		b.WriteString(p)
	}
	// id is unique, so ORDER BY ... LIMIT picks a deterministic row set.
	orderable = append(orderable, t.Name+".id")
	b.WriteString(" ORDER BY ")
	b.WriteString(g.orderBy(orderable))
	if g.rnd.Intn(3) == 0 {
		fmt.Fprintf(&b, " LIMIT %d", 1+g.rnd.Intn(10))
	}
	return b.String()
}

func (g *Generator) aggregateSelect(t Table) string {
	var expr string
	switch g.rnd.Intn(5) {
	case 0:
		expr = "COUNT(*)"
	case 1, 2:
		expr = fmt.Sprintf("SUM(%s.%s)", t.Name, g.numericColumn(t).Name)
	case 3:
		expr = fmt.Sprintf("AVG(%s.%s)", t.Name, g.numericColumn(t).Name)
	default:
		fn := "MIN"
		if g.rnd.Intn(2) == 0 {
			fn = "MAX"
		}
		col := t.Columns[g.rnd.Intn(len(t.Columns))]
		expr = fmt.Sprintf("%s(%s.%s)", fn, t.Name, col.Name)
	}

	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(expr)
	b.WriteString(" FROM ")
	b.WriteString(t.Name)
	if p := g.whereClause(t, nil); p != "" {
		b.WriteString(" WHERE ")
		b.WriteString(p)
	}
	return b.String()
}

func (g *Generator) numericExpr(t Table) string {
	col := g.numericColumn(t)
	op := [...]string{"+", "-", "*"}[g.rnd.Intn(3)]
	lit := 1 + g.rnd.Intn(10)
	return fmt.Sprintf("%s.%s %s %d", t.Name, col.Name, op, lit)
}

func (g *Generator) joinTable(t Table) *Table {
	if len(g.tables) < 2 || g.rnd.Intn(5) >= 2 {
		return nil
	}
	for {
		candidate := g.tables[g.rnd.Intn(len(g.tables))]
		if candidate.Name != t.Name {
			return &candidate
		}
	}
}

func (g *Generator) whereClause(t Table, join *Table) string {
	if g.rnd.Intn(10) < 3 {
		return ""
	}
	count := 1
	if g.rnd.Intn(3) == 0 {
		count = 2
	}
	parts := make([]string, 0, count)
	for i := 0; i < count; i++ {
		tbl := t
		if join != nil && g.rnd.Intn(2) == 0 {
			tbl = *join
		}
		parts = append(parts, g.predicate(tbl))
	}
	if len(parts) == 1 {
		if g.rnd.Intn(5) == 0 {
			return "NOT (" + parts[0] + ")"
		}
		return parts[0]
	}
	op := " AND "
	if g.rnd.Intn(2) == 0 {
		op = " OR "
	}
	return strings.Join(parts, op)
}

func (g *Generator) predicate(t Table) string {
	col := t.Columns[g.rnd.Intn(len(t.Columns))]
	ref := t.Name + "." + col.Name

	switch col.Kind {
	case Int, Float:
		switch g.rnd.Intn(5) {
		case 0:
			lo := g.rnd.Intn(10) - 5
			return fmt.Sprintf("%s BETWEEN %d AND %d", ref, lo, lo+5+g.rnd.Intn(10))
		case 1:
			return fmt.Sprintf("%s IN (%d, %d, %d)", ref,
				g.rnd.Intn(20)-5, g.rnd.Intn(20)-5, g.rnd.Intn(20)-5)
		case 2:
			return ref + " IS NULL"
		default:
			return fmt.Sprintf("%s %s %d", ref, g.comparison(), g.rnd.Intn(20)-5)
		}
	case Text:
		if g.rnd.Intn(3) == 0 {
			pattern := [...]string{"s%", "n%", "%1", "s0_", "%a%"}[g.rnd.Intn(5)]
			return fmt.Sprintf("%s LIKE '%s'", ref, pattern)
		}
		lit := [...]string{"alpha", "beta", "s01", "n02"}[g.rnd.Intn(4)]
		return fmt.Sprintf("%s %s '%s'", ref, g.comparison(), lit)
	case Date:
		lit := [...]string{"2024-01-01", "2024-01-10", "2024-02-01 00:00:00"}[g.rnd.Intn(3)]
		return fmt.Sprintf("%s %s '%s'", ref, g.comparison(), lit)
	default:
		return ref + " IS NOT NULL"
	}
}

func (g *Generator) orderBy(refs []string) string {
	n := 1 + g.rnd.Intn(2)
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ref := refs[g.rnd.Intn(len(refs))]
		if g.rnd.Intn(2) == 0 {
			ref += " DESC"
		}
		parts = append(parts, ref)
	}
	return strings.Join(parts, ", ")
}

func (g *Generator) numericColumn(t Table) Column {
	numeric := make([]Column, 0, len(t.Columns))
	for _, c := range t.Columns {
		if c.Kind == Int || c.Kind == Float {
			numeric = append(numeric, c)
		}
	}
	return numeric[g.rnd.Intn(len(numeric))]
}

func (g *Generator) comparison() string {
	return [...]string{"=", "!=", "<", "<=", ">", ">="}[g.rnd.Intn(6)]
}
