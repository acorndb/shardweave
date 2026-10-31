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

func distinctFields() []result.Field {
	return []result.Field{
		{Name: "user_id", OrgName: "user_id", Type: result.TypeLong},
		{Name: "amount", OrgName: "amount", Type: result.TypeLong},
	}
}

func TestDistinct_MultiShard(t *testing.T) {
	fields := distinctFields()
	streams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{
			{[]byte("1"), []byte("50")},
			{[]byte("2"), []byte("200")},
		}},
		&scriptStream{fields: fields, rows: []result.Row{
			{[]byte("1"), []byte("50")},
			{[]byte("2"), []byte("50")},
		}},
	}
	s, err := merge.Distinct(streams, fields)
	require.NoError(t, err)
	require.Equal(t, fields, s.Fields())
	require.Equal(t, [][]string{
		{"1", "50"},
		{"2", "200"},
		{"2", "50"},
	}, collectValues(t, s))
	_, err = s.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestDistinct_NullAndEmpty(t *testing.T) {
	fields := []result.Field{{Name: "v", OrgName: "v", Type: result.TypeLong}}
	streams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{{nil}, {[]byte("1")}}},
		&scriptStream{fields: fields, rows: []result.Row{{nil}}},
	}
	s, err := merge.Distinct(streams, fields)
	require.NoError(t, err)
	require.Equal(t, [][]string{{"<nil>"}, {"1"}}, collectValues(t, s))

	empty, err := merge.Distinct(nil, fields)
	require.NoError(t, err)
	_, err = empty.Next()
	require.ErrorIs(t, err, io.EOF)
}

func TestDistinct_RejectsString(t *testing.T) {
	fields := []result.Field{{Name: "name", OrgName: "name", Type: result.TypeVarString}}
	_, err := merge.Distinct(nil, fields)
	var me *errs.MySQL
	require.ErrorAs(t, err, &me)
	require.Equal(t, uint16(1235), me.Number)
}

func TestDistinct_Close(t *testing.T) {
	fields := distinctFields()
	a := &scriptStream{fields: fields}
	b := &scriptStream{fields: fields}
	s, err := merge.Distinct([]result.Stream{a, b}, fields)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	require.Equal(t, 1, a.closes)
	require.Equal(t, 1, b.closes)
}

func TestMerge_DistinctFactory(t *testing.T) {
	fields := distinctFields()
	s, err := merge.Merge([]result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("1"), []byte("50")}}},
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("1"), []byte("50")}}},
	}, fields, plan.MergeSpec{Distinct: true})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"1", "50"}}, collectValues(t, s))
}
