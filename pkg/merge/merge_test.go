package merge_test

import (
	"errors"
	"io"
	"testing"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/merge"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/result"
	"github.com/stretchr/testify/require"
)

type scriptStream struct {
	fields []result.Field
	rows   []result.Row
	err    error

	i      int
	reads  int
	closes int
}

func (s *scriptStream) Fields() []result.Field { return s.fields }

func (s *scriptStream) Next() (result.Row, error) {
	s.reads++
	if s.i < len(s.rows) {
		row := s.rows[s.i]
		s.i++
		return row, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	return nil, io.EOF
}

func (s *scriptStream) Close() error {
	s.closes++
	return nil
}

func rows(vals ...string) []result.Row {
	out := make([]result.Row, 0, len(vals))
	for _, v := range vals {
		out = append(out, result.Row{[]byte(v)})
	}
	return out
}

func collect(t *testing.T, s result.Stream) []string {
	t.Helper()
	var got []string
	for {
		row, err := s.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		require.NoError(t, err)
		got = append(got, string(row[0].([]byte)))
	}
}

func TestConcat_OrderAndFields(t *testing.T) {
	a := &scriptStream{
		fields: []result.Field{{Name: "id"}},
		rows:   rows("1", "2"),
	}
	b := &scriptStream{rows: rows("3")}

	s := merge.Concat([]result.Stream{a, b})
	require.Equal(t, []result.Field{{Name: "id"}}, s.Fields())
	require.Equal(t, []string{"1", "2", "3"}, collect(t, s))
	require.NoError(t, s.Close())
	require.Equal(t, 1, a.closes)
	require.Equal(t, 1, b.closes)
}

func TestConcat_EmptyAndNilStreams(t *testing.T) {
	a := &scriptStream{fields: []result.Field{{Name: "id"}}}
	b := &scriptStream{rows: rows("1")}

	s := merge.Concat([]result.Stream{a, nil, b})
	require.Equal(t, []result.Field{{Name: "id"}}, s.Fields())
	require.Equal(t, []string{"1"}, collect(t, s))
}

func TestConcat_ErrorPropagates(t *testing.T) {
	boom := errors.New("boom")
	a := &scriptStream{rows: rows("1"), err: boom}
	s := merge.Concat([]result.Stream{a})

	row, err := s.Next()
	require.NoError(t, err)
	require.Equal(t, result.Row{[]byte("1")}, row)

	_, err = s.Next()
	require.ErrorIs(t, err, boom)
}

func TestConcat_CloseIdempotent(t *testing.T) {
	a := &scriptStream{}
	s := merge.Concat([]result.Stream{a})
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	require.Equal(t, 1, a.closes)
}

func TestLimit_OffsetAndCount(t *testing.T) {
	up := &scriptStream{rows: rows("1", "2", "3", "4")}
	s := merge.Limit(up, 1, 2)

	require.Equal(t, []string{"2", "3"}, collect(t, s))
	require.Equal(t, 3, up.reads, "limit must not read past the last returned row")
}

func TestLimit_ZeroCount(t *testing.T) {
	up := &scriptStream{rows: rows("1")}
	s := merge.Limit(up, 0, 0)
	_, err := s.Next()
	require.ErrorIs(t, err, io.EOF)
	require.Zero(t, up.reads, "zero limit must not read upstream")
}

func TestLimit_UpstreamError(t *testing.T) {
	boom := errors.New("boom")
	up := &scriptStream{rows: rows("1"), err: boom}
	s := merge.Limit(up, 1, 2)
	_, err := s.Next()
	require.ErrorIs(t, err, boom)
}

func TestLimit_FieldsAndClose(t *testing.T) {
	up := &scriptStream{fields: []result.Field{{Name: "id"}}, rows: rows("1")}
	s := merge.Limit(up, 0, 1)
	require.Equal(t, []result.Field{{Name: "id"}}, s.Fields())
	require.NoError(t, s.Close())
	require.Equal(t, 1, up.closes)
}

func TestMerge_Factory(t *testing.T) {
	fields := []result.Field{{Name: "id", OrgName: "id", Type: 0x03}}
	mk := func() []result.Stream {
		return []result.Stream{
			&scriptStream{fields: fields, rows: rows("2")},
			&scriptStream{fields: fields, rows: rows("1")},
		}
	}

	concat, err := merge.Merge(mk(), fields, plan.MergeSpec{})
	require.NoError(t, err)
	require.Equal(t, []string{"2", "1"}, collect(t, concat))

	ordered, err := merge.Merge(mk(), fields, plan.MergeSpec{
		OrderBy: []plan.OrderKey{{Column: 0}},
		Limit:   &plan.Limit{Count: 1},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"1"}, collect(t, ordered))

	cases := []plan.MergeSpec{
		{GroupBy: []int{0}},
		{Distinct: true},
		{Output: []int{0}},
		{
			Aggregates: []plan.AggregateSpec{{Func: plan.AggCount, Column: 0, Output: 0}},
			OrderBy:    []plan.OrderKey{{Column: 0}},
		},
	}
	for _, spec := range cases {
		_, err := merge.Merge(mk(), fields, spec)
		var me *errs.MySQL
		require.ErrorAs(t, err, &me)
		require.Equal(t, uint16(1235), me.Number)
	}
}

func TestValidate(t *testing.T) {
	require.NoError(t, merge.Validate(plan.MergeSpec{
		OrderBy: []plan.OrderKey{{Column: 0}},
		Limit:   &plan.Limit{Count: 1},
	}))
	for _, spec := range []plan.MergeSpec{
		{GroupBy: []int{0}},
		{Distinct: true},
		{Output: []int{0}},
		{Aggregates: []plan.AggregateSpec{{Func: plan.AggCount}}, OrderBy: []plan.OrderKey{{Column: 0}}},
	} {
		var me *errs.MySQL
		require.ErrorAs(t, merge.Validate(spec), &me)
		require.Equal(t, uint16(1235), me.Number)
	}
}
