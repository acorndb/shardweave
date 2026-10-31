package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEncodeOK_Minimal(t *testing.T) {
	p := EncodeOK(0, 0, 2, 0)
	require.Equal(t, byte(OKHeader), p[0])
	require.GreaterOrEqual(t, len(p), 7)
}

func TestEncodeERR(t *testing.T) {
	p := EncodeERR(1045, "28000", "Access denied")
	require.Equal(t, byte(ErrHeader), p[0])
	require.Contains(t, string(p), "Access denied")
	require.Contains(t, string(p), "#28000")
}

func TestEncodeColumnCount(t *testing.T) {
	require.Equal(t, []byte{0x01}, EncodeColumnCount(1))
	require.Equal(t, []byte{0xfc, 0x00, 0x01}, EncodeColumnCount(256))
}

func TestEncodeColumnDefinition(t *testing.T) {
	got := EncodeColumnDefinition(Column{
		Name:         "1",
		Charset:      0x003f,
		ColumnLength: 1,
		Type:         0x08,
	})
	want := []byte{
		0x03, 'd', 'e', 'f',
		0x00, 0x00, 0x00,
		0x01, '1',
		0x00,
		0x0c,
		0x3f, 0x00,
		0x01, 0x00, 0x00, 0x00,
		0x08,
		0x00, 0x00,
		0x00,
		0x00, 0x00,
	}
	require.Equal(t, want, got)
}

func TestEncodeTextRow(t *testing.T) {
	got := EncodeTextRow([][]byte{[]byte("1"), nil, {}})
	require.Equal(t, []byte{0x01, '1', 0xfb, 0x00}, got)
}

func TestEncodeEOF(t *testing.T) {
	// CLIENT_DEPRECATE_EOF: OK packet with the 0xfe header.
	require.Equal(t, []byte{0xfe, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00},
		EncodeEOF(true, 0x0002, 0))

	// Classic EOF: header, warnings, status.
	require.Equal(t, []byte{0xfe, 0x01, 0x00, 0x02, 0x00},
		EncodeEOF(false, 0x0002, 1))
}
