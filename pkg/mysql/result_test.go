package mysql

import (
	"net"
	"testing"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
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

func TestWriteERR(t *testing.T) {
	srv, cli := net.Pipe()
	defer func() { _ = srv.Close() }()
	defer func() { _ = cli.Close() }()

	front := packet.NewConn(srv)
	rd := packet.NewConn(cli)
	wrote := make(chan error, 1)
	go func() {
		wrote <- WriteERR(front, errs.Unsupported("nope"))
	}()

	data, err := rd.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, EncodeERR(errs.ErNotSupportedYet, errs.StateSyntax, "nope"), data)
	require.NoError(t, <-wrote)
}

func TestWriteERR_NilArgs(t *testing.T) {
	require.Error(t, WriteERR(nil, errs.Unsupported("x")))

	srv, cli := net.Pipe()
	defer func() { _ = srv.Close() }()
	defer func() { _ = cli.Close() }()
	front := packet.NewConn(srv)
	rd := packet.NewConn(cli)
	wrote := make(chan error, 1)
	go func() { wrote <- WriteERR(front, nil) }()

	data, err := rd.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, EncodeERR(errs.ErUnknownError, errs.StateGeneral, "unknown error"), data)
	require.NoError(t, <-wrote)
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

func writeAndRead(t *testing.T, n int, write func(*packet.Conn) error) [][]byte {
	t.Helper()
	srv, cli := net.Pipe()
	t.Cleanup(func() { _ = srv.Close(); _ = cli.Close() })
	front := packet.NewConn(srv)
	rd := packet.NewConn(cli)
	done := make(chan error, 1)
	go func() { done <- write(front) }()

	pkts := make([][]byte, 0, n)
	for range n {
		data, err := rd.ReadPacket()
		require.NoError(t, err)
		pkts = append(pkts, data)
	}
	require.NoError(t, <-done)
	return pkts
}

func TestWriteOK(t *testing.T) {
	pkts := writeAndRead(t, 1, func(front *packet.Conn) error {
		return WriteOK(front, 2, 9, 0x0002, 1)
	})
	require.Equal(t, EncodeOK(2, 9, 0x0002, 1), pkts[0])
	require.Error(t, WriteOK(nil, 0, 0, 0, 0))
}

func TestWriteResultHeader(t *testing.T) {
	cols := []Column{{Name: "id", Charset: 63, Type: 0x03}, {Name: "amount", Charset: 63, Type: 0x03}}
	pkts := writeAndRead(t, 1+len(cols), func(front *packet.Conn) error {
		return WriteResultHeader(front, cols)
	})
	require.Equal(t, EncodeColumnCount(2), pkts[0])
	for i, col := range cols {
		require.Equal(t, EncodeColumnDefinition(col), pkts[i+1])
	}
	require.Error(t, WriteResultHeader(nil, cols))
}

func TestWriteEOF(t *testing.T) {
	pkts := writeAndRead(t, 1, func(front *packet.Conn) error {
		return WriteEOF(front, true, 0x0002, 0)
	})
	require.Equal(t, EncodeEOF(true, 0x0002, 0), pkts[0])
	require.Error(t, WriteEOF(nil, true, 0, 0))
}
