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

func aggFields() []result.Field {
	return []result.Field{
		{Name: "COUNT(*)", OrgName: "COUNT(*)", Type: 0x08},
		{Name: "SUM(amount)", OrgName: "SUM(amount)", Type: typeNewDec},
		{Name: "MIN(amount)", OrgName: "MIN(amount)", Type: typeLong},
		{Name: "MAX(amount)", OrgName: "MAX(amount)", Type: typeLong},
	}
}

func aggSpecs() []plan.AggregateSpec {
	return []plan.AggregateSpec{
		{Func: plan.AggCount, Column: 0, Output: 0},
		{Func: plan.AggSum, Column: 1, Output: 1},
		{Func: plan.AggMin, Column: 2, Output: 2},
		{Func: plan.AggMax, Column: 3, Output: 3},
	}
}

func TestAggregate_MultiShard(t *testing.T) {
	fields := aggFields()
	streams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("2"), []byte("50"), []byte("50"), []byte("50")}}},
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("3"), []byte("200"), []byte("70"), []byte("200")}}},
	}
	s, err := merge.Aggregate(streams, fields, aggSpecs())
	require.NoError(t, err)
	require.Equal(t, fields, s.Fields())
	require.Equal(t, [][]string{{"5", "250", "50", "200"}}, collectValues(t, s))
	_, err = s.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestAggregate_Nulls(t *testing.T) {
	fields := aggFields()
	streams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("1"), nil, nil, nil}}},
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("1"), []byte("10"), []byte("5"), []byte("5")}}},
	}
	s, err := merge.Aggregate(streams, fields, aggSpecs())
	require.NoError(t, err)
	require.Equal(t, [][]string{{"2", "10", "5", "5"}}, collectValues(t, s))
}

func TestAggregate_AllNull(t *testing.T) {
	fields := aggFields()
	s, err := merge.Aggregate(nil, fields, aggSpecs())
	require.NoError(t, err)
	require.Equal(t, [][]string{{"0", "<nil>", "<nil>", "<nil>"}}, collectValues(t, s))
}

func TestAggregate_SumScaleAndFloat(t *testing.T) {
	decFields := []result.Field{{Name: "SUM(v)", OrgName: "SUM(v)", Type: typeNewDec, Decimals: 2}}
	dec, err := merge.Aggregate([]result.Stream{
		&scriptStream{fields: decFields, rows: []result.Row{{[]byte("1.10")}}},
		&scriptStream{fields: decFields, rows: []result.Row{{[]byte("2.20")}}},
	}, decFields, []plan.AggregateSpec{{Func: plan.AggSum, Column: 0, Output: 0}})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"3.30"}}, collectValues(t, dec))

	fltFields := []result.Field{{Name: "SUM(v)", OrgName: "SUM(v)", Type: 0x05}}
	flt, err := merge.Aggregate([]result.Stream{
		&scriptStream{fields: fltFields, rows: []result.Row{{[]byte("1.5")}}},
		&scriptStream{fields: fltFields, rows: []result.Row{{[]byte("2.5")}}},
	}, fltFields, []plan.AggregateSpec{{Func: plan.AggSum, Column: 0, Output: 0}})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"4"}}, collectValues(t, flt))
}

func TestAggregate_RejectsUnsupported(t *testing.T) {
	strFields := []result.Field{{Name: "MIN(v)", OrgName: "MIN(v)", Type: typeVarStr}}
	_, err := merge.Aggregate(nil, strFields, []plan.AggregateSpec{{Func: plan.AggMin, Column: 0, Output: 0}})
	var me *errs.MySQL
	require.ErrorAs(t, err, &me)
	require.Equal(t, uint16(1235), me.Number)

	fields := aggFields()
	_, err = merge.Aggregate(nil, fields, []plan.AggregateSpec{{Func: plan.AggregateFunc(99), Column: 0, Output: 0}})
	require.ErrorAs(t, err, &me)

	_, err = merge.Aggregate(nil, fields, []plan.AggregateSpec{{Func: plan.AggCount, Column: 0, Output: 2}})
	require.ErrorAs(t, err, &me)
}

func TestAggregate_Close(t *testing.T) {
	fields := aggFields()
	a := &scriptStream{fields: fields}
	b := &scriptStream{fields: fields}
	s, err := merge.Aggregate([]result.Stream{a, b}, fields, aggSpecs())
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	require.Equal(t, 1, a.closes)
	require.Equal(t, 1, b.closes)
}

func avgFields(avgType byte, avgDecimals byte, sumType byte) []result.Field {
	return []result.Field{
		{Name: "AVG(amount)", OrgName: "AVG(amount)", Type: avgType, Decimals: avgDecimals},
		{Name: "__sw_avg_sum_0", OrgName: "SUM(amount)", Type: sumType},
		{Name: "__sw_avg_count_0", OrgName: "COUNT(amount)", Type: 0x08},
	}
}

func TestAggregate_Avg(t *testing.T) {
	fields := avgFields(typeNewDec, 4, typeNewDec)
	streams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("2.5000"), []byte("5"), []byte("2")}}},
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("4.0000"), []byte("8"), []byte("2")}}},
	}
	s, err := merge.Aggregate(streams, fields, []plan.AggregateSpec{{
		Func: plan.AggAvg, Column: 1, CountColumn: 2, Output: 0,
	}})
	require.NoError(t, err)
	require.Equal(t, fields[:1], s.Fields())
	require.Equal(t, [][]string{{"3.2500"}}, collectValues(t, s))
}

func TestAggregate_AvgNulls(t *testing.T) {
	fields := avgFields(typeNewDec, 4, typeNewDec)
	streams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{{nil, nil, []byte("0")}}},
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("5.0000"), []byte("10"), []byte("2")}}},
	}
	s, err := merge.Aggregate(streams, fields, []plan.AggregateSpec{{
		Func: plan.AggAvg, Column: 1, CountColumn: 2, Output: 0,
	}})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"5.0000"}}, collectValues(t, s))
}

func TestAggregate_AvgAllNull(t *testing.T) {
	fields := avgFields(typeNewDec, 4, typeNewDec)
	s, err := merge.Aggregate([]result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{{nil, nil, []byte("0")}}},
	}, fields, []plan.AggregateSpec{{
		Func: plan.AggAvg, Column: 1, CountColumn: 2, Output: 0,
	}})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"<nil>"}}, collectValues(t, s))
}

func TestAggregate_AvgFloat(t *testing.T) {
	fields := avgFields(0x05, 0, 0x05)
	s, err := merge.Aggregate([]result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("1.5"), []byte("1.5"), []byte("1")}}},
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("2.5"), []byte("2.5"), []byte("1")}}},
	}, fields, []plan.AggregateSpec{{Func: plan.AggAvg, Column: 1, CountColumn: 2, Output: 0}})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"2"}}, collectValues(t, s))
}

func TestAggregate_AvgInvalidCountColumn(t *testing.T) {
	fields := avgFields(typeNewDec, 4, typeNewDec)
	var me *errs.MySQL
	_, err := merge.Aggregate(nil, fields, []plan.AggregateSpec{{
		Func: plan.AggAvg, Column: 1, CountColumn: 9, Output: 0,
	}})
	require.ErrorAs(t, err, &me)
	require.Equal(t, uint16(1235), me.Number)
}
