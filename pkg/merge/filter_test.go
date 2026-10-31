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

func TestFilter_Having(t *testing.T) {
	fields := []result.Field{
		{Name: "user_id", OrgName: "user_id", Type: result.TypeLong},
		{Name: "c", OrgName: "COUNT(*)", Type: result.TypeLongLong},
		{Name: "s", OrgName: "SUM(amount)", Type: result.TypeNewDecimal},
	}
	up := &scriptStream{fields: fields, rows: []result.Row{
		{[]byte("1"), []byte("1"), []byte("50")},
		{[]byte("2"), []byte("2"), []byte("250")},
		{[]byte("3"), []byte("3"), []byte("80")},
	}}
	s, err := merge.Filter(up, fields, []plan.HavingPredicate{
		{Column: 1, Op: plan.HavingGT, Value: "1"},
		{Column: 2, Op: plan.HavingGE, Value: "100"},
	})
	require.NoError(t, err)
	require.Equal(t, fields, s.Fields())
	require.Equal(t, [][]string{{"2", "2", "250"}}, collectValues(t, s))
	require.NoError(t, s.Close())
	require.Equal(t, 1, up.closes)
}

func TestFilter_NullAndOps(t *testing.T) {
	fields := []result.Field{{Name: "v", OrgName: "v", Type: result.TypeLong}}
	up := &scriptStream{fields: fields, rows: []result.Row{{nil}, {[]byte("1")}, {[]byte("2")}, {[]byte("3")}}}
	s, err := merge.Filter(up, fields, []plan.HavingPredicate{{Column: 0, Op: plan.HavingNE, Value: "2"}})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"1"}, {"3"}}, collectValues(t, s))

	empty, err := merge.Filter(&scriptStream{fields: fields}, fields, nil)
	require.NoError(t, err)
	_, err = empty.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestFilter_RejectsUnsupported(t *testing.T) {
	var me *errs.MySQL
	strFields := []result.Field{{Name: "name", OrgName: "name", Type: result.TypeVarString}}
	_, err := merge.Filter(&scriptStream{fields: strFields}, strFields,
		[]plan.HavingPredicate{{Column: 0, Op: plan.HavingGT, Value: "1"}})
	require.ErrorAs(t, err, &me)
	require.Equal(t, uint16(1235), me.Number)

	fields := []result.Field{{Name: "v", OrgName: "v", Type: result.TypeLong}}
	_, err = merge.Filter(&scriptStream{fields: fields}, fields,
		[]plan.HavingPredicate{{Column: 0, Op: plan.HavingGT, Value: "x"}})
	require.ErrorAs(t, err, &me)

	_, err = merge.Filter(&scriptStream{fields: fields}, fields,
		[]plan.HavingPredicate{{Column: 9, Op: plan.HavingGT, Value: "1"}})
	require.ErrorAs(t, err, &me)
}

func TestMerge_GroupHaving(t *testing.T) {
	fields := []result.Field{
		{Name: "user_id", OrgName: "user_id", Type: result.TypeLong},
		{Name: "c", OrgName: "COUNT(*)", Type: result.TypeLongLong},
	}
	streams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("1"), []byte("1")}}},
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("2"), []byte("2")}}},
	}
	s, err := merge.Merge(streams, fields, plan.MergeSpec{
		GroupBy: []int{0},
		Aggregates: []plan.AggregateSpec{
			{Func: plan.AggCount, Column: 1, Output: 1},
		},
		Output: []int{0, 1},
		Having: []plan.HavingPredicate{{Column: 1, Op: plan.HavingGT, Value: "1"}},
	})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"2", "2"}}, collectValues(t, s))
}
