package merge_test

import (
	"io"
	"testing"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/merge"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/result"
	"github.com/stretchr/testify/require"
)

func groupFields() []result.Field {
	return []result.Field{
		{Name: "user_id", OrgName: "user_id", Type: result.TypeLong},
		{Name: "COUNT(*)", OrgName: "COUNT(*)", Type: result.TypeLongLong},
		{Name: "SUM(amount)", OrgName: "SUM(amount)", Type: result.TypeNewDecimal},
		{Name: "MIN(amount)", OrgName: "MIN(amount)", Type: result.TypeLong},
		{Name: "MAX(amount)", OrgName: "MAX(amount)", Type: result.TypeLong},
		{Name: "AVG(amount)", OrgName: "AVG(amount)", Type: result.TypeNewDecimal, Decimals: 4},
		{Name: "__sw_avg_sum_0", OrgName: "SUM(amount)", Type: result.TypeNewDecimal},
		{Name: "__sw_avg_count_0", OrgName: "COUNT(amount)", Type: result.TypeLongLong},
	}
}

func groupSpecs() []plan.AggregateSpec {
	return []plan.AggregateSpec{
		{Func: plan.AggCount, Column: 1, Output: 1},
		{Func: plan.AggSum, Column: 2, Output: 2},
		{Func: plan.AggMin, Column: 3, Output: 3},
		{Func: plan.AggMax, Column: 4, Output: 4},
		{Func: plan.AggAvg, Column: 6, CountColumn: 7, Output: 5},
	}
}

func TestGroup_MultiShard(t *testing.T) {
	fields := groupFields()
	streams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{
			{[]byte("1"), []byte("2"), []byte("30"), []byte("10"), []byte("20"), []byte("15.0000"), []byte("30"), []byte("2")},
			{[]byte("2"), []byte("1"), []byte("100"), []byte("100"), []byte("100"), []byte("100.0000"), []byte("100"), []byte("1")},
		}},
		&scriptStream{fields: fields, rows: []result.Row{
			{[]byte("1"), []byte("1"), []byte("5"), []byte("5"), []byte("5"), []byte("5.0000"), []byte("5"), []byte("1")},
			{[]byte("3"), []byte("1"), []byte("7"), []byte("7"), []byte("7"), []byte("7.0000"), []byte("7"), []byte("1")},
		}},
	}
	s, err := merge.Group(streams, fields, []int{0}, groupSpecs(), []int{0, 1, 2, 3, 4, 5})
	require.NoError(t, err)
	require.Equal(t, fields[:6], s.Fields())
	require.Equal(t, [][]string{
		{"1", "3", "35", "5", "20", "11.6667"},
		{"2", "1", "100", "100", "100", "100.0000"},
		{"3", "1", "7", "7", "7", "7.0000"},
	}, collectValues(t, s))
	_, err = s.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestGroup_NullKeyAndNoAggregates(t *testing.T) {
	fields := []result.Field{{Name: "user_id", OrgName: "user_id", Type: result.TypeLong}}
	streams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{{nil}, {[]byte("1")}}},
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("1")}, {nil}}},
	}
	s, err := merge.Group(streams, fields, []int{0}, nil, []int{0})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"<nil>"}, {"1"}}, collectValues(t, s))
}

func TestGroup_EmptyInput(t *testing.T) {
	fields := groupFields()
	s, err := merge.Group(nil, fields, []int{0}, groupSpecs(), []int{0, 1, 2, 3, 4, 5})
	require.NoError(t, err)
	_, err = s.Next()
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, fields[:6], s.Fields())
}

func TestGroup_RejectsUnsupported(t *testing.T) {
	var me *errs.MySQL
	strFields := []result.Field{{Name: "name", OrgName: "name", Type: result.TypeVarString}}
	_, err := merge.Group(nil, strFields, []int{0}, nil, []int{0})
	require.ErrorAs(t, err, &me)
	require.Equal(t, uint16(1235), me.Number)

	fields := groupFields()
	_, err = merge.Group(nil, fields, []int{0}, groupSpecs(), []int{0, 9})
	require.ErrorAs(t, err, &me)
}

func TestGroup_Close(t *testing.T) {
	fields := groupFields()
	a := &scriptStream{fields: fields}
	b := &scriptStream{fields: fields}
	s, err := merge.Group([]result.Stream{a, b}, fields, []int{0}, groupSpecs(), []int{0, 1, 2, 3, 4, 5})
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	require.Equal(t, 1, a.closes)
	require.Equal(t, 1, b.closes)
}

func TestMerge_GroupFactory(t *testing.T) {
	fields := groupFields()
	streams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{
			{[]byte("1"), []byte("1"), []byte("10"), []byte("10"), []byte("10"), []byte("10.0000"), []byte("10"), []byte("1")},
		}},
		&scriptStream{fields: fields, rows: []result.Row{
			{[]byte("1"), []byte("1"), []byte("5"), []byte("5"), []byte("5"), []byte("5.0000"), []byte("5"), []byte("1")},
		}},
	}
	s, err := merge.Merge(streams, fields, plan.MergeSpec{
		GroupBy:    []int{0},
		Aggregates: groupSpecs(),
		Output:     []int{0, 1, 2, 3, 4, 5},
	})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"1", "2", "15", "5", "10", "7.5000"}}, collectValues(t, s))
}

func TestGroup_FloatAndNulls(t *testing.T) {
	fields := []result.Field{
		{Name: "user_id", OrgName: "user_id", Type: result.TypeLong},
		{Name: "SUM(v)", OrgName: "SUM(v)", Type: result.TypeDouble},
		{Name: "AVG(v)", OrgName: "AVG(v)", Type: result.TypeDouble},
		{Name: "__sw_avg_count_0", OrgName: "COUNT(v)", Type: result.TypeLongLong},
	}
	streams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{
			{[]byte("1"), nil, nil, []byte("0")},
			{[]byte("1"), []byte("1.5"), []byte("1.5"), []byte("1")},
		}},
		&scriptStream{fields: fields, rows: []result.Row{
			{[]byte("2"), []byte("2.5"), []byte("2.5"), []byte("1")},
		}},
	}
	s, err := merge.Group(streams, fields, []int{0}, []plan.AggregateSpec{
		{Func: plan.AggSum, Column: 1, Output: 1},
		{Func: plan.AggAvg, Column: 2, CountColumn: 3, Output: 2},
	}, []int{0, 1, 2})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"1", "1.5", "1.5"}, {"2", "2.5", "2.5"}}, collectValues(t, s))
}

func TestGroup_ValidationErrors(t *testing.T) {
	fields := groupFields()
	var me *errs.MySQL
	cases := []struct {
		groupBy []int
		specs   []plan.AggregateSpec
		output  []int
	}{
		{nil, nil, []int{0}},
		{[]int{0}, nil, nil},
		{[]int{9}, nil, []int{0}},
		{[]int{0}, nil, []int{0, 9}},
		{[]int{0}, []plan.AggregateSpec{{Func: plan.AggCount, Column: 0, Output: 9}}, []int{0}},
		{[]int{0}, []plan.AggregateSpec{{Func: plan.AggCount, Column: 0, Output: 0, Distinct: true}}, []int{0}},
		{[]int{0}, []plan.AggregateSpec{{Func: plan.AggregateFunc(99), Column: 0, Output: 0}}, []int{0}},
		{[]int{0}, []plan.AggregateSpec{{Func: plan.AggAvg, Column: 1, CountColumn: 9, Output: 0}}, []int{0}},
	}
	for _, tc := range cases {
		_, err := merge.Group(nil, fields, tc.groupBy, tc.specs, tc.output)
		require.ErrorAs(t, err, &me)
		require.Equal(t, uint16(1235), me.Number)
	}
}
