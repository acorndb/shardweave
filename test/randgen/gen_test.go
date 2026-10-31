package randgen

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func testTables() []Table {
	return []Table{
		{Name: "rnd_int", Columns: []Column{
			{Name: "id", Kind: Int},
			{Name: "a", Kind: Int},
			{Name: "b", Kind: Int},
			{Name: "c", Kind: Float},
		}},
		{Name: "rnd_str", Columns: []Column{
			{Name: "id", Kind: Int},
			{Name: "s", Kind: Text},
			{Name: "t", Kind: Text},
			{Name: "d", Kind: Date},
			{Name: "dt", Kind: Date},
			{Name: "f", Kind: Float},
		}},
		{Name: "rnd_null", Columns: []Column{
			{Name: "id", Kind: Int},
			{Name: "v", Kind: Int},
			{Name: "s", Kind: Text},
		}},
	}
}

func TestNewRejectsInvalidSchema(t *testing.T) {
	require.Panics(t, func() { New(1, nil) })
	require.Panics(t, func() { New(1, []Table{{Name: "t"}}) })
	require.Panics(t, func() {
		New(1, []Table{{Name: "t", Columns: []Column{{Name: "v", Kind: Int}}}})
	})
	require.Panics(t, func() {
		New(1, []Table{{Name: "t", Columns: []Column{{Name: "id", Kind: Text}}}})
	})
}

func TestSelectDeterministic(t *testing.T) {
	a := New(42, testTables())
	b := New(42, testTables())
	for i := 0; i < 50; i++ {
		require.Equal(t, a.Select(), b.Select())
	}
}

func TestSelectShape(t *testing.T) {
	g := New(7, testTables())
	seen := map[string]bool{}
	for i := 0; i < 300; i++ {
		q := g.Select()
		require.True(t, strings.HasPrefix(q, "SELECT "), q)
		require.Contains(t, q, " FROM ")
		require.NotContains(t, q, ";")
		upper := strings.ToUpper(q)
		for _, banned := range []string{"NOW(", "RAND(", "UUID(", "SLEEP(", "@@", "CONNECTION_ID("} {
			require.NotContains(t, upper, banned, q)
		}
		for _, table := range testTables() {
			if strings.Contains(q, "FROM "+table.Name) || strings.Contains(q, "JOIN "+table.Name) {
				seen[table.Name] = true
			}
		}
	}
	for _, table := range testTables() {
		require.True(t, seen[table.Name], "table %s 未被覆盖", table.Name)
	}
}

func TestSelectFeatures(t *testing.T) {
	g := New(11, testTables())
	var aggregate, join, where, order, limit bool
	for i := 0; i < 400; i++ {
		upper := strings.ToUpper(g.Select())
		aggregate = aggregate || strings.Contains(upper, "COUNT(") ||
			strings.Contains(upper, "SUM(") || strings.Contains(upper, "AVG(") ||
			strings.Contains(upper, "MIN(") || strings.Contains(upper, "MAX(")
		join = join || strings.Contains(upper, " JOIN ")
		where = where || strings.Contains(upper, " WHERE ")
		order = order || strings.Contains(upper, " ORDER BY ")
		limit = limit || strings.Contains(upper, " LIMIT ")
	}
	require.True(t, aggregate, "未生成聚合查询")
	require.True(t, join, "未生成 JOIN")
	require.True(t, where, "未生成 WHERE")
	require.True(t, order, "未生成 ORDER BY")
	require.True(t, limit, "未生成 LIMIT")
}
