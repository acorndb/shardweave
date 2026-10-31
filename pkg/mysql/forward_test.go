package mysql_test

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/stretchr/testify/require"
)

func pipePair(t *testing.T) (src, dst, backendWriter, frontReader *packet.Conn) {
	t.Helper()
	bw, br := net.Pipe()
	t.Cleanup(func() { _ = bw.Close(); _ = br.Close() })
	fw, fr := net.Pipe()
	t.Cleanup(func() { _ = fw.Close(); _ = fr.Close() })
	return packet.NewConn(br), packet.NewConn(fw), packet.NewConn(bw), packet.NewConn(fr)
}

func writePackets(t *testing.T, w *packet.Conn, packets [][]byte) <-chan error {
	t.Helper()
	ch := make(chan error, 1)
	go func() {
		for _, p := range packets {
			buf := make([]byte, 4+len(p))
			copy(buf[4:], p)
			if err := w.WritePacket(buf); err != nil {
				ch <- err
				return
			}
		}
		ch <- nil
	}()
	return ch
}

func drainFront(t *testing.T, r *packet.Conn, n int) <-chan [][]byte {
	t.Helper()
	ch := make(chan [][]byte, 1)
	go func() {
		out := make([][]byte, 0, n)
		for i := 0; i < n; i++ {
			d, err := r.ReadPacket()
			if err != nil {
				ch <- nil
				return
			}
			out = append(out, d)
		}
		ch <- out
	}()
	return ch
}

func TestForwardResponse_OK(t *testing.T) {
	src, dst, bw, fr := pipePair(t)
	ok := mysql.EncodeOK(1, 0, mysql.ServerStatusAutocommit, 0)
	werr := writePackets(t, bw, [][]byte{ok})
	front := drainFront(t, fr, 1)

	status, err := mysql.ForwardResponse(dst, src, true)
	require.NoError(t, err)
	require.Equal(t, mysql.ServerStatusAutocommit, status)
	require.NoError(t, <-werr)
	got := <-front
	require.Equal(t, [][]byte{ok}, got)
}

func TestForwardResponse_OK_InTrans(t *testing.T) {
	src, dst, bw, fr := pipePair(t)
	want := mysql.ServerStatusAutocommit | mysql.ServerStatusInTrans
	ok := mysql.EncodeOK(0, 0, want, 0)
	werr := writePackets(t, bw, [][]byte{ok})
	front := drainFront(t, fr, 1)

	status, err := mysql.ForwardResponse(dst, src, true)
	require.NoError(t, err)
	require.Equal(t, want, status)
	require.NoError(t, <-werr)
	require.NotNil(t, <-front)
}

func TestForwardResponse_ERR(t *testing.T) {
	src, dst, bw, fr := pipePair(t)
	er := mysql.EncodeERR(1064, "42000", "You have an error")
	werr := writePackets(t, bw, [][]byte{er})
	front := drainFront(t, fr, 1)

	status, err := mysql.ForwardResponse(dst, src, true)
	require.NoError(t, err)
	require.Equal(t, uint16(0), status)
	require.NoError(t, <-werr)
	got := <-front
	require.Equal(t, [][]byte{er}, got)
}

func TestForwardResponseResult_ERR(t *testing.T) {
	src, dst, bw, fr := pipePair(t)
	er := mysql.EncodeERR(1064, "42000", "You have an error")
	werr := writePackets(t, bw, [][]byte{er})
	front := drainFront(t, fr, 1)

	status, serverErr, err := mysql.ForwardResponseResult(dst, src, true)
	require.NoError(t, err)
	require.True(t, serverErr)
	require.Equal(t, uint16(0), status)
	require.NoError(t, <-werr)
	require.Equal(t, [][]byte{er}, <-front)
}

func TestForwardResponseInfo_OKMetadata(t *testing.T) {
	src, dst, bw, fr := pipePair(t)
	ok := mysql.EncodeOK(3, 42, mysql.ServerStatusAutocommit, 1)
	werr := writePackets(t, bw, [][]byte{ok})
	front := drainFront(t, fr, 1)

	info, err := mysql.ForwardResponseInfo(dst, src, true)
	require.NoError(t, err)
	require.False(t, info.ServerErr)
	require.False(t, info.ResultSet)
	require.Equal(t, mysql.ServerStatusAutocommit, info.Status)
	require.Equal(t, uint64(3), info.AffectedRows)
	require.Equal(t, uint64(42), info.LastInsertID)
	require.Equal(t, uint16(1), info.Warnings)

	require.NoError(t, <-werr)
	require.Equal(t, [][]byte{ok}, <-front)
}

func TestForwardResponseInfo_ResultSet(t *testing.T) {
	src, dst, bw, fr := pipePair(t)
	colCount := []byte{0x01}
	colDef := []byte{
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
	row := []byte{0x01, '1'}
	eofOK := []byte{0xfe, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}
	packets := [][]byte{colCount, colDef, row, eofOK}

	werr := writePackets(t, bw, packets)
	front := drainFront(t, fr, len(packets))

	info, err := mysql.ForwardResponseInfo(dst, src, true)
	require.NoError(t, err)
	require.False(t, info.ServerErr)
	require.True(t, info.ResultSet)
	require.Equal(t, mysql.ServerStatusAutocommit, info.Status)
	require.Zero(t, info.AffectedRows)
	require.Zero(t, info.LastInsertID)

	require.NoError(t, <-werr)
	require.Equal(t, packets, <-front)
}

func TestForwardResponseResult_OK(t *testing.T) {
	src, dst, bw, fr := pipePair(t)
	ok := mysql.EncodeOK(1, 0, mysql.ServerStatusAutocommit, 0)
	werr := writePackets(t, bw, [][]byte{ok})
	front := drainFront(t, fr, 1)

	status, serverErr, err := mysql.ForwardResponseResult(dst, src, true)
	require.NoError(t, err)
	require.False(t, serverErr)
	require.Equal(t, mysql.ServerStatusAutocommit, status)
	require.NoError(t, <-werr)
	require.Equal(t, [][]byte{ok}, <-front)
}

func TestForwardResponse_ResultsetDeprecateEOF(t *testing.T) {
	src, dst, bw, fr := pipePair(t)

	// Real MySQL 8 + CLIENT_DEPRECATE_EOF for SELECT 1:
	// colcount, coldef, row, then 0xfe OK-replacing-EOF (no intermediate marker).
	colCount := []byte{0x01}
	colDef := []byte{
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
	row := []byte{0x01, '1'}
	eofOK := []byte{0xfe, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}
	packets := [][]byte{colCount, colDef, row, eofOK}

	werr := writePackets(t, bw, packets)
	front := drainFront(t, fr, len(packets))

	status, err := mysql.ForwardResponse(dst, src, true)
	require.NoError(t, err)
	require.Equal(t, mysql.ServerStatusAutocommit, status) // eofOK status=0x0002
	require.NoError(t, <-werr)
	got := <-front
	require.Equal(t, packets, got)
}

func TestForwardPrepareResponse(t *testing.T) {
	src, dst, bw, fr := pipePair(t)

	// COM_STMT_PREPARE_OK: 1 param, 1 column; deprecateEOF → no intermediate EOFs.
	prepareOK := []byte{
		0x00,                   // OK
		0x01, 0x00, 0x00, 0x00, // statement_id=1
		0x01, 0x00, // num_columns=1
		0x01, 0x00, // num_params=1
		0x00,       // reserved
		0x00, 0x00, // warnings
	}
	paramDef := []byte{
		0x03, 'd', 'e', 'f', 0x00, 0x00, 0x00, 0x01, '?', 0x00, 0x0c,
		0x3f, 0x00, 0x01, 0x00, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	colDef := []byte{
		0x03, 'd', 'e', 'f', 0x00, 0x00, 0x00, 0x01, '1', 0x00, 0x0c,
		0x3f, 0x00, 0x01, 0x00, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	packets := [][]byte{prepareOK, paramDef, colDef}

	werr := writePackets(t, bw, packets)
	front := drainFront(t, fr, len(packets))

	require.NoError(t, mysql.ForwardPrepareResponse(dst, src, true))
	require.NoError(t, <-werr)
	require.Equal(t, packets, <-front)
}

func TestForwardPrepareResponseWithID(t *testing.T) {
	src, dst, bw, fr := pipePair(t)

	prepareOK := []byte{
		0x00,                   // OK
		0x01, 0x00, 0x00, 0x00, // backend statement_id=1
		0x01, 0x00, // num_columns=1
		0x01, 0x00, // num_params=1
		0x00,       // reserved
		0x00, 0x00, // warnings
	}
	paramDef := []byte{
		0x03, 'd', 'e', 'f', 0x00, 0x00, 0x00, 0x01, '?', 0x00, 0x0c,
		0x3f, 0x00, 0x01, 0x00, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	colDef := []byte{
		0x03, 'd', 'e', 'f', 0x00, 0x00, 0x00, 0x01, '1', 0x00, 0x0c,
		0x3f, 0x00, 0x01, 0x00, 0x00, 0x00, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00,
	}

	werr := writePackets(t, bw, [][]byte{prepareOK, paramDef, colDef})
	front := drainFront(t, fr, 3)

	backendID, serverErr, err := mysql.ForwardPrepareResponseWithID(dst, src, true, 42)
	require.NoError(t, err)
	require.False(t, serverErr)
	require.Equal(t, uint32(1), backendID)
	require.NoError(t, <-werr)

	got := <-front
	require.Len(t, got, 3)
	require.Equal(t, byte(mysql.OKHeader), got[0][0])
	require.Equal(t, uint32(42), binary.LittleEndian.Uint32(got[0][1:5]))
	require.Equal(t, paramDef, got[1])
	require.Equal(t, colDef, got[2])
}

func TestForwardResponse_MultiResult(t *testing.T) {
	src, dst, bw, fr := pipePair(t)

	// Two OK packets; first has SERVER_MORE_RESULTS_EXISTS.
	okMore := mysql.EncodeOK(0, 0, mysql.ServerStatusAutocommit|mysql.ServerMoreResultsExists, 0)
	okDone := mysql.EncodeOK(0, 0, mysql.ServerStatusAutocommit, 0)
	packets := [][]byte{okMore, okDone}

	werr := writePackets(t, bw, packets)
	front := drainFront(t, fr, len(packets))

	status, err := mysql.ForwardResponse(dst, src, true)
	require.NoError(t, err)
	require.Equal(t, mysql.ServerStatusAutocommit, status)
	require.NoError(t, <-werr)
	require.Equal(t, packets, <-front)
}

func TestForwardResponse_MultiResult_LastStatusInTrans(t *testing.T) {
	src, dst, bw, fr := pipePair(t)

	okMore := mysql.EncodeOK(0, 0, mysql.ServerStatusAutocommit|mysql.ServerMoreResultsExists, 0)
	okDone := mysql.EncodeOK(0, 0, mysql.ServerStatusAutocommit|mysql.ServerStatusInTrans, 0)
	packets := [][]byte{okMore, okDone}

	werr := writePackets(t, bw, packets)
	front := drainFront(t, fr, len(packets))

	status, err := mysql.ForwardResponse(dst, src, true)
	require.NoError(t, err)
	require.Equal(t, mysql.ServerStatusAutocommit|mysql.ServerStatusInTrans, status)
	require.NoError(t, <-werr)
	require.Equal(t, packets, <-front)
}

func TestForwardResponse_ResultsetClassicEOF(t *testing.T) {
	src, dst, bw, fr := pipePair(t)

	colCount := []byte{0x01}
	colDef := []byte{
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
	eofCols := []byte{0xfe, 0x00, 0x00, 0x02, 0x00}
	row := []byte{0x01, '1'}
	eofRows := []byte{0xfe, 0x00, 0x00, 0x02, 0x00}
	packets := [][]byte{colCount, colDef, eofCols, row, eofRows}

	werr := writePackets(t, bw, packets)
	front := drainFront(t, fr, len(packets))

	status, err := mysql.ForwardResponse(dst, src, false)
	require.NoError(t, err)
	require.Equal(t, mysql.ServerStatusAutocommit, status)
	require.NoError(t, <-werr)
	got := <-front
	require.Equal(t, packets, got)
}
