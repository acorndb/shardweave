package backend

import (
	"io"
	"net"
	"testing"

	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/acorndb/shardweave/pkg/result"
	"github.com/stretchr/testify/require"
)

func pipePacket(t *testing.T) (srv, cli *packet.Conn) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	return packet.NewConn(server), packet.NewConn(client)
}

func writeBackendPackets(t *testing.T, pc *packet.Conn, packets [][]byte) <-chan error {
	t.Helper()
	ch := make(chan error, 1)
	go func() {
		pc.ResetSequence()
		for _, p := range packets {
			buf := make([]byte, 4+len(p))
			copy(buf[4:], p)
			if err := pc.WritePacket(buf); err != nil {
				ch <- err
				return
			}
		}
		ch <- nil
	}()
	return ch
}

func TestDecodedStream_TextResult(t *testing.T) {
	srv, cli := pipePacket(t)
	packets := [][]byte{
		mysql.EncodeColumnCount(2),
		mysql.EncodeColumnDefinition(mysql.Column{Name: "id", Type: 0x08, Charset: 0x3f, ColumnLength: 11}),
		mysql.EncodeColumnDefinition(mysql.Column{Name: "name", Type: 0xfd, Charset: 0x21, ColumnLength: 32}),
		mysql.EncodeTextRow([][]byte{[]byte("1"), []byte("a")}),
		mysql.EncodeTextRow([][]byte{[]byte("2"), nil}),
		mysql.EncodeEOF(true, mysql.ServerStatusAutocommit, 0),
	}
	werr := writeBackendPackets(t, srv, packets)

	stream, err := newDecodedStream(cli, true)
	require.NoError(t, err)

	require.Len(t, stream.Fields(), 2)
	require.Equal(t, "id", stream.Fields()[0].Name)
	require.Equal(t, "name", stream.Fields()[1].Name)

	row, err := stream.Next()
	require.NoError(t, err)
	require.Equal(t, result.Row{[]byte("1"), []byte("a")}, row)

	row, err = stream.Next()
	require.NoError(t, err)
	require.Equal(t, result.Row{[]byte("2"), nil}, row)

	_, err = stream.Next()
	require.ErrorIs(t, err, io.EOF)
	require.NoError(t, <-werr)
}

func TestDecodedStream_LegacyEOF(t *testing.T) {
	srv, cli := pipePacket(t)
	packets := [][]byte{
		mysql.EncodeColumnCount(1),
		mysql.EncodeColumnDefinition(mysql.Column{Name: "v", Type: 0xfd, Charset: 0x21}),
		mysql.EncodeEOF(false, mysql.ServerStatusAutocommit, 0),
		mysql.EncodeTextRow([][]byte{[]byte("x")}),
		mysql.EncodeEOF(false, mysql.ServerStatusAutocommit, 0),
	}
	werr := writeBackendPackets(t, srv, packets)

	stream, err := newDecodedStream(cli, false)
	require.NoError(t, err)
	row, err := stream.Next()
	require.NoError(t, err)
	require.Equal(t, result.Row{[]byte("x")}, row)
	_, err = stream.Next()
	require.ErrorIs(t, err, io.EOF)
	require.NoError(t, <-werr)
}

func TestDecodedStream_NoResultSet(t *testing.T) {
	srv, cli := pipePacket(t)
	werr := writeBackendPackets(t, srv, [][]byte{mysql.EncodeOK(0, 0, mysql.ServerStatusAutocommit, 0)})

	_, err := newDecodedStream(cli, true)
	require.ErrorIs(t, err, ErrNoResultSet)
	require.NoError(t, <-werr)
}

func TestDecodedStream_ServerError(t *testing.T) {
	srv, cli := pipePacket(t)
	werr := writeBackendPackets(t, srv, [][]byte{mysql.EncodeERR(1146, "42S02", "no table")})

	_, err := newDecodedStream(cli, true)
	var se *mysql.ServerError
	require.ErrorAs(t, err, &se)
	require.Equal(t, uint16(1146), se.Number)
	require.NoError(t, <-werr)
}
