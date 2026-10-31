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

func TestSort_MultiKeyNullDesc(t *testing.T) {
	fields := []result.Field{{Name: "id", OrgName: "id", Type: result.TypeLong}, {Name: "v", OrgName: "v", Type: result.TypeLong}}
	up := &scriptStream{fields: fields, rows: []result.Row{
		{[]byte("2"), []byte("5")},
		{[]byte("1"), nil},
		{[]byte("1"), []byte("9")},
		{[]byte("2"), []byte("7")},
	}}
	s, err := merge.Sort(up, fields, []plan.OrderKey{
		{Column: 0},
		{Column: 1, Desc: true},
	})
	require.NoError(t, err)
	require.Equal(t, fields, s.Fields())
	require.Equal(t, [][]string{
		{"1", "9"},
		{"1", "<nil>"},
		{"2", "7"},
		{"2", "5"},
	}, collectValues(t, s))
	require.NoError(t, s.Close())
	require.Equal(t, 1, up.closes)
}

func TestSort_RejectsUnsupported(t *testing.T) {
	strFields := []result.Field{{Name: "name", OrgName: "name", Type: result.TypeVarString}}
	_, err := merge.Sort(&scriptStream{fields: strFields}, strFields, []plan.OrderKey{{Column: 0}})
	var me *errs.MySQL
	require.ErrorAs(t, err, &me)
	require.Equal(t, uint16(1235), me.Number)

	fields := []result.Field{{Name: "id", OrgName: "id", Type: result.TypeLong}}
	_, err = merge.Sort(&scriptStream{fields: fields}, fields, []plan.OrderKey{{Column: -1, Kind: plan.OrderByName, Name: "missing"}})
	require.ErrorAs(t, err, &me)
	require.Contains(t, me.Message, "missing")

	_, err = merge.Sort(&scriptStream{fields: fields}, fields, []plan.OrderKey{{Column: 2}})
	require.ErrorAs(t, err, &me)
}

func TestSort_UpstreamErrorAndEmpty(t *testing.T) {
	fields := []result.Field{{Name: "id", OrgName: "id", Type: result.TypeLong}}
	boom := errors.New("boom")
	up := &scriptStream{fields: fields, rows: []result.Row{{[]byte("1")}}, err: boom}
	s, err := merge.Sort(up, fields, []plan.OrderKey{{Column: 0}})
	require.NoError(t, err)
	_, err = s.Next()
	require.ErrorIs(t, err, boom)

	empty, err := merge.Sort(&scriptStream{fields: fields}, fields, []plan.OrderKey{{Column: 0}})
	require.NoError(t, err)
	_, err = empty.Next()
	require.ErrorIs(t, err, io.EOF)
}
