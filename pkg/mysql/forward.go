package mysql

import (
	"encoding/binary"
	"fmt"

	"github.com/acorndb/shardweave/pkg/mysql/packet"
)

// ForwardResponse copies one MySQL command response from src to dst,
// adjusting packet framing via each Conn's sequence counters.
// Handles OK/ERR, text/binary result sets, and multi-result (MORE_RESULTS_EXISTS).
// Returns the SERVER_STATUS from the last OK/EOF terminator (0 on ERR-only).
//
// Resultset framing matches go-mysql-org/go-mysql client/resp.go:
//   - CLIENT_DEPRECATE_EOF: no intermediate marker after columns; rows end with
//     a short 0xfe packet (OK that replaces EOF).
//   - otherwise: EOF after columns, then rows, then EOF.
func ForwardResponse(dst, src *packet.Conn, deprecateEOF bool) (serverStatus uint16, err error) {
	status, _, err := ForwardResponseResult(dst, src, deprecateEOF)
	return status, err
}

// ResponseInfo describes the observable outcome of one command response.
// ResultSet is true when the last result was a result set (SELECT/SHOW/...),
// false for an OK/ERR-only response.
type ResponseInfo struct {
	Status       uint16
	ServerErr    bool
	AffectedRows uint64
	LastInsertID uint64
	Warnings     uint16
	ResultSet    bool
}

// ForwardResponseResult is ForwardResponse plus whether the backend returned an
// ERR packet. Callers that mutate session state must skip the mutation when
// serverErr is true: an ERR packet has no SERVER_STATUS field, so status is 0
// and is not a valid state transition.
func ForwardResponseResult(dst, src *packet.Conn, deprecateEOF bool) (serverStatus uint16, serverErr bool, err error) {
	info, err := ForwardResponseInfo(dst, src, deprecateEOF)
	return info.Status, info.ServerErr, err
}

// ForwardResponseInfo copies one command response and returns its OK-packet
// metadata (affected rows, last insert id, warnings, result-set type).
func ForwardResponseInfo(dst, src *packet.Conn, deprecateEOF bool) (ResponseInfo, error) {
	var out ResponseInfo
	for {
		one, more, err := forwardOne(dst, src, deprecateEOF)
		if err != nil {
			return ResponseInfo{}, err
		}
		if one.ServerErr {
			out.ServerErr = true
		} else {
			out.Status = one.Status
		}
		if one.LastInsertID != 0 {
			out.LastInsertID = one.LastInsertID
		}
		if !one.ResultSet && !one.ServerErr {
			out.AffectedRows = one.AffectedRows
			out.Warnings = one.Warnings
		}
		out.ResultSet = one.ResultSet
		if !more {
			return out, nil
		}
	}
}

// ForwardPrepareResponse copies COM_STMT_PREPARE_OK plus optional param/column
// definition sections (go-mysql / MySQL binary prepare reply).
func ForwardPrepareResponse(dst, src *packet.Conn, deprecateEOF bool) error {
	data, err := src.ReadPacket()
	if err != nil {
		return err
	}
	if err := writePayload(dst, data); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if data[0] == ErrHeader {
		return nil
	}
	if data[0] != OKHeader || len(data) < 12 {
		return fmt.Errorf("mysql: expected COM_STMT_PREPARE_OK, got hdr=0x%02x len=%d", data[0], len(data))
	}
	numColumns := binary.LittleEndian.Uint16(data[5:7])
	numParams := binary.LittleEndian.Uint16(data[7:9])

	if numParams > 0 {
		if err := forwardDefinitions(dst, src, uint64(numParams), deprecateEOF); err != nil {
			return err
		}
	}
	if numColumns > 0 {
		if err := forwardDefinitions(dst, src, uint64(numColumns), deprecateEOF); err != nil {
			return err
		}
	}
	return nil
}

func forwardOne(dst, src *packet.Conn, deprecateEOF bool) (info ResponseInfo, more bool, err error) {
	data, err := src.ReadPacket()
	if err != nil {
		return ResponseInfo{}, false, err
	}
	if err := writePayload(dst, data); err != nil {
		return ResponseInfo{}, false, err
	}
	if len(data) == 0 {
		return ResponseInfo{}, false, nil
	}
	switch data[0] {
	case ErrHeader:
		return ResponseInfo{ServerErr: true}, false, nil
	case OKHeader:
		info.Status, info.AffectedRows, info.LastInsertID, info.Warnings = parseOK(data)
		return info, statusHasMore(info.Status), nil
	case 0xfb: // LOCAL INFILE — not supported in P1/P2
		return ResponseInfo{}, false, fmt.Errorf("mysql: LOCAL INFILE not supported")
	}

	info.ResultSet = true
	cols, _, err := readLenEncInt(data)
	if err != nil {
		return ResponseInfo{}, false, err
	}
	if err := forwardDefinitions(dst, src, cols, deprecateEOF); err != nil {
		return ResponseInfo{}, false, err
	}

	for {
		row, err := src.ReadPacket()
		if err != nil {
			return ResponseInfo{}, false, err
		}
		if err := writePayload(dst, row); err != nil {
			return ResponseInfo{}, false, err
		}
		if len(row) == 0 {
			continue
		}
		if row[0] == ErrHeader {
			return ResponseInfo{ResultSet: true, ServerErr: true}, false, nil
		}
		// Terminator: classic EOF or OK-replacing-EOF (0xfe, len < 9).
		if row[0] == EOFHeader && len(row) < 9 {
			info.Status = eofStatus(row, deprecateEOF)
			return info, statusHasMore(info.Status), nil
		}
	}
}

func forwardDefinitions(dst, src *packet.Conn, n uint64, deprecateEOF bool) error {
	for i := uint64(0); i < n; i++ {
		col, err := src.ReadPacket()
		if err != nil {
			return err
		}
		if err := writePayload(dst, col); err != nil {
			return err
		}
	}
	if deprecateEOF {
		return nil
	}
	mark, err := src.ReadPacket()
	if err != nil {
		return err
	}
	if err := writePayload(dst, mark); err != nil {
		return err
	}
	if len(mark) == 0 || mark[0] != EOFHeader || len(mark) >= 9 {
		return fmt.Errorf("mysql: expected EOF after definitions, got hdr=0x%02x len=%d", mark[0], len(mark))
	}
	return nil
}

func statusHasMore(status uint16) bool {
	return status&ServerMoreResultsExists != 0
}

// parseOK extracts affected rows, last insert id, status and warnings from a
// PROTOCOL_41 OK payload.
func parseOK(data []byte) (status uint16, affectedRows, lastInsertID uint64, warnings uint16) {
	if len(data) < 1 || data[0] != OKHeader {
		return 0, 0, 0, 0
	}
	pos := 1
	var adv int
	var err error
	affectedRows, adv, err = readLenEncInt(data[pos:])
	if err != nil {
		return 0, 0, 0, 0
	}
	pos += adv
	lastInsertID, adv, err = readLenEncInt(data[pos:])
	if err != nil {
		return 0, 0, 0, 0
	}
	pos += adv
	if pos+2 > len(data) {
		return 0, 0, 0, 0
	}
	status = binary.LittleEndian.Uint16(data[pos : pos+2])
	pos += 2
	if pos+2 <= len(data) {
		warnings = binary.LittleEndian.Uint16(data[pos : pos+2])
	}
	return status, affectedRows, lastInsertID, warnings
}

// eofStatus extracts SERVER_STATUS from classic EOF or deprecate-EOF terminator.
func eofStatus(data []byte, deprecateEOF bool) uint16 {
	if len(data) == 0 || data[0] != EOFHeader {
		return 0
	}
	if deprecateEOF {
		// OK-replacing-EOF: 0xfe, affected_rows, last_insert_id, status...
		pos := 1
		_, adv, err := readLenEncInt(data[pos:])
		if err != nil {
			return 0
		}
		pos += adv
		_, adv, err = readLenEncInt(data[pos:])
		if err != nil {
			return 0
		}
		pos += adv
		if pos+2 > len(data) {
			return 0
		}
		return binary.LittleEndian.Uint16(data[pos : pos+2])
	}
	// Classic EOF: 0xfe, warnings(2), status(2)
	if len(data) < 5 {
		return 0
	}
	return binary.LittleEndian.Uint16(data[3:5])
}

func writePayload(dst *packet.Conn, payload []byte) error {
	buf := make([]byte, 4+len(payload))
	copy(buf[4:], payload)
	return dst.WritePacket(buf)
}

func readLenEncInt(b []byte) (n uint64, adv int, err error) {
	if len(b) == 0 {
		return 0, 0, fmt.Errorf("mysql: empty length-encoded int")
	}
	switch b[0] {
	case 0xfb:
		return 0, 1, nil
	case 0xfc:
		if len(b) < 3 {
			return 0, 0, fmt.Errorf("mysql: truncated lenenc")
		}
		return uint64(b[1]) | uint64(b[2])<<8, 3, nil
	case 0xfd:
		if len(b) < 4 {
			return 0, 0, fmt.Errorf("mysql: truncated lenenc")
		}
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16, 4, nil
	case 0xfe:
		if len(b) < 9 {
			return 0, 0, fmt.Errorf("mysql: truncated lenenc")
		}
		n = uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16 | uint64(b[4])<<24 |
			uint64(b[5])<<32 | uint64(b[6])<<40 | uint64(b[7])<<48 | uint64(b[8])<<56
		return n, 9, nil
	default:
		return uint64(b[0]), 1, nil
	}
}
