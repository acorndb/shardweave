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

const (
	typeLong     byte   = 0x03
	typeDateTime byte   = 0x0c
	typeNewDec   byte   = 0xf6
	typeVarStr   byte   = 0xfd
	flagUnsigned uint16 = 0x20
)

func numField(name string) result.Field {
	return result.Field{Name: name, OrgName: name, Type: typeLong}
}

func collectValues(t *testing.T, s result.Stream) [][]string {
	t.Helper()
	var out [][]string
	for {
		row, err := s.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		require.NoError(t, err)
		cells := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				cells[i] = "<nil>"
			} else {
				cells[i] = string(v.([]byte))
			}
		}
		out = append(out, cells)
	}
}

func TestOrdered_AscendingAndDescending(t *testing.T) {
	fields := []result.Field{numField("id"), numField("amount")}
	asc, err := merge.Ordered([]result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{
			{[]byte("1"), []byte("10")},
			{[]byte("3"), []byte("30")},
		}},
		&scriptStream{fields: fields, rows: []result.Row{
			{[]byte("2"), []byte("20")},
			{[]byte("4"), []byte("40")},
		}},
	}, fields, []plan.OrderKey{{Column: 0}})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"1", "10"}, {"2", "20"}, {"3", "30"}, {"4", "40"}}, collectValues(t, asc))

	desc, err := merge.Ordered([]result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{
			{[]byte("3"), []byte("30")},
			{[]byte("1"), []byte("10")},
		}},
		&scriptStream{fields: fields, rows: []result.Row{
			{[]byte("4"), []byte("40")},
			{[]byte("2"), []byte("20")},
		}},
	}, fields, []plan.OrderKey{{Column: 0, Desc: true}})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"4", "40"}, {"3", "30"}, {"2", "20"}, {"1", "10"}}, collectValues(t, desc))
}

func TestOrdered_MultiKey(t *testing.T) {
	fields := []result.Field{numField("id"), numField("amount")}
	streams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{
			{[]byte("1"), []byte("5")},
			{[]byte("2"), []byte("1")},
		}},
		&scriptStream{fields: fields, rows: []result.Row{
			{[]byte("1"), []byte("9")},
			{[]byte("2"), []byte("7")},
		}},
	}
	s, err := merge.Ordered(streams, fields, []plan.OrderKey{
		{Column: 0},
		{Column: 1, Desc: true},
	})
	require.NoError(t, err)
	require.Equal(t, [][]string{
		{"1", "9"}, {"1", "5"}, {"2", "7"}, {"2", "1"},
	}, collectValues(t, s))
}

func TestOrdered_Nulls(t *testing.T) {
	fields := []result.Field{numField("amount")}
	ascStreams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{{nil}, {[]byte("2")}}},
		&scriptStream{fields: fields, rows: []result.Row{{nil}, {[]byte("1")}}},
	}
	asc, err := merge.Ordered(ascStreams, fields, []plan.OrderKey{{Column: 0}})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"<nil>"}, {"<nil>"}, {"1"}, {"2"}}, collectValues(t, asc))

	descStreams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("2")}, {nil}}},
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("1")}, {nil}}},
	}
	desc, err := merge.Ordered(descStreams, fields, []plan.OrderKey{{Column: 0, Desc: true}})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"2"}, {"1"}, {"<nil>"}, {"<nil>"}}, collectValues(t, desc))
}

func TestOrdered_ResolveNameAndOrgName(t *testing.T) {
	fields := []result.Field{{Name: "x", OrgName: "id", Type: typeLong}}
	mk := func() []result.Stream {
		return []result.Stream{
			&scriptStream{fields: fields, rows: []result.Row{{[]byte("2")}}},
			&scriptStream{fields: fields, rows: []result.Row{{[]byte("1")}}},
		}
	}

	for _, name := range []string{"id", "x"} {
		s, err := merge.Ordered(mk(), fields, []plan.OrderKey{{Column: -1, Name: name}})
		require.NoError(t, err)
		require.Equal(t, [][]string{{"1"}, {"2"}}, collectValues(t, s), name)
	}
}

func TestOrdered_Unsigned(t *testing.T) {
	fields := []result.Field{{Name: "id", OrgName: "id", Type: typeLong, Flags: flagUnsigned}}
	streams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("18446744073709551615")}}},
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("1")}}},
	}
	s, err := merge.Ordered(streams, fields, []plan.OrderKey{{Column: 0}})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"1"}, {"18446744073709551615"}}, collectValues(t, s))
}

func TestOrdered_DecimalAndDateTime(t *testing.T) {
	decFields := []result.Field{{Name: "v", OrgName: "v", Type: typeNewDec}}
	dec, err := merge.Ordered([]result.Stream{
		&scriptStream{fields: decFields, rows: []result.Row{{[]byte("1.2")}, {[]byte("10")}}},
		&scriptStream{fields: decFields, rows: []result.Row{{[]byte("1.10")}}},
	}, decFields, []plan.OrderKey{{Column: 0}})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"1.10"}, {"1.2"}, {"10"}}, collectValues(t, dec))

	dtFields := []result.Field{{Name: "ts", OrgName: "ts", Type: typeDateTime}}
	dt, err := merge.Ordered([]result.Stream{
		&scriptStream{fields: dtFields, rows: []result.Row{{[]byte("2024-01-02 00:00:00")}}},
		&scriptStream{fields: dtFields, rows: []result.Row{{[]byte("2024-01-01 00:00:00")}}},
	}, dtFields, []plan.OrderKey{{Column: 0}})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"2024-01-01 00:00:00"}, {"2024-01-02 00:00:00"}}, collectValues(t, dt))
}

func TestOrdered_EqualKeysTieBreak(t *testing.T) {
	fields := []result.Field{numField("id")}
	streams := []result.Stream{
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("1")}}},
		&scriptStream{fields: fields, rows: []result.Row{{[]byte("1")}}},
	}
	s, err := merge.Ordered(streams, fields, []plan.OrderKey{{Column: 0}})
	require.NoError(t, err)
	rows := collectValues(t, s)
	require.Len(t, rows, 2)
}

func TestOrdered_RejectsUnsupportedTypeAndKeys(t *testing.T) {
	badType := []result.Field{{Name: "v", OrgName: "v", Type: typeVarStr}}
	_, err := merge.Ordered([]result.Stream{
		&scriptStream{fields: badType, rows: []result.Row{{[]byte("a")}}},
	}, badType, []plan.OrderKey{{Column: 0}})
	var me *errs.MySQL
	require.ErrorAs(t, err, &me)
	require.Equal(t, uint16(1235), me.Number)

	fields := []result.Field{numField("id")}
	_, err = merge.Ordered([]result.Stream{&scriptStream{fields: fields}}, fields,
		[]plan.OrderKey{{Column: -1, Name: "missing"}})
	require.ErrorAs(t, err, &me)
	require.Contains(t, me.Message, "missing")

	_, err = merge.Ordered([]result.Stream{&scriptStream{fields: fields}}, fields,
		[]plan.OrderKey{{Column: 2}})
	require.ErrorAs(t, err, &me)
}

func TestOrdered_ErrorAndClose(t *testing.T) {
	fields := []result.Field{numField("id")}
	boom := errors.New("boom")
	a := &scriptStream{fields: fields, rows: []result.Row{{[]byte("1")}}, err: boom}
	b := &scriptStream{fields: fields, rows: []result.Row{{[]byte("2")}}}
	s, err := merge.Ordered([]result.Stream{a, b}, fields, []plan.OrderKey{{Column: 0}})
	require.NoError(t, err)

	row, err := s.Next()
	require.NoError(t, err)
	require.Equal(t, result.Row{[]byte("1")}, row)
	_, err = s.Next()
	require.ErrorIs(t, err, boom)

	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	require.Equal(t, 1, a.closes)
	require.Equal(t, 1, b.closes)
}

func TestOrdered_Empty(t *testing.T) {
	fields := []result.Field{numField("id")}
	s, err := merge.Ordered(nil, fields, []plan.OrderKey{{Column: 0}})
	require.NoError(t, err)
	_, err = s.Next()
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, fields, s.Fields())
}
