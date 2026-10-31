package mysql

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/errs"

	"github.com/stretchr/testify/require"
)

func TestDecodeLenEncInt(t *testing.T) {
	cases := []struct {
		data []byte
		want uint64
		adv  int
	}{
		{[]byte{0x05}, 5, 1},
		{[]byte{0xfc, 0x00, 0x01}, 256, 3},
		{[]byte{0xfd, 0x01, 0x00, 0x01}, 65537, 4},
	}
	for _, tc := range cases {
		got, adv, err := DecodeLenEncInt(tc.data)
		require.NoError(t, err)
		require.Equal(t, tc.want, got)
		require.Equal(t, tc.adv, adv)
	}
	_, _, err := DecodeLenEncInt(nil)
	require.Error(t, err)
}

func TestDecodeColumnRoundTrip(t *testing.T) {
	in := Column{
		Schema:       "app",
		Table:        "orders",
		OrgTable:     "orders",
		Name:         "id",
		OrgName:      "id",
		Charset:      0x002d,
		ColumnLength: 11,
		Type:         0x03,
		Flags:        0x0003,
		Decimals:     0,
	}
	got, err := DecodeColumn(EncodeColumnDefinition(in))
	require.NoError(t, err)
	require.Equal(t, in, got)
}

func TestDecodeColumnRejectsTruncated(t *testing.T) {
	data := EncodeColumnDefinition(Column{Name: "id"})
	_, err := DecodeColumn(data[:len(data)-1])
	require.Error(t, err)
}

func TestDecodeTextRow(t *testing.T) {
	data := EncodeTextRow([][]byte{[]byte("abc"), nil, {}})
	cells, err := DecodeTextRow(data, 3)
	require.NoError(t, err)
	require.Len(t, cells, 3)
	require.Equal(t, []byte("abc"), cells[0])
	require.Nil(t, cells[1])
	require.Empty(t, cells[2])
	require.NotNil(t, cells[2], "empty value must stay distinct from NULL")

	_, err = DecodeTextRow([]byte{0x05, 'a'}, 1)
	require.Error(t, err)
}

func TestDecodeServerError(t *testing.T) {
	err := DecodeServerError(EncodeERR(1146, "42S02", "Table 'x' doesn't exist"))
	var se *errs.MySQL
	require.ErrorAs(t, err, &se)
	require.Equal(t, uint16(1146), se.Number)
	require.Equal(t, "42S02", se.SQLState)
	require.Contains(t, se.Error(), "doesn't exist")
}
