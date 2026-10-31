package mysql

import (
	"encoding/binary"
	"fmt"

	"github.com/acorndb/prism/pkg/mysql/packet"
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
	for {
		var more bool
		var status uint16
		more, status, err = forwardOne(dst, src, deprecateEOF)
		if err != nil {
			return 0, err
		}
		serverStatus = status
		if !more {
			return serverStatus, nil
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

func forwardOne(dst, src *packet.Conn, deprecateEOF bool) (more bool, status uint16, err error) {
	data, err := src.ReadPacket()
	if err != nil {
		return false, 0, err
	}
	if err := writePayload(dst, data); err != nil {
		return false, 0, err
	}
	if len(data) == 0 {
		return false, 0, nil
	}
	switch data[0] {
	case ErrHeader:
		return false, 0, nil
	case OKHeader:
		status = okStatus(data)
		return statusHasMore(status), status, nil
	case 0xfb: // LOCAL INFILE — not supported in P1/P2
		return false, 0, fmt.Errorf("mysql: LOCAL INFILE not supported")
	}

	cols, _, err := readLenEncInt(data)
	if err != nil {
		return false, 0, err
	}
	if err := forwardDefinitions(dst, src, cols, deprecateEOF); err != nil {
		return false, 0, err
	}

	for {
		row, err := src.ReadPacket()
		if err != nil {
			return false, 0, err
		}
		if err := writePayload(dst, row); err != nil {
			return false, 0, err
		}
		if len(row) == 0 {
			continue
		}
		if row[0] == ErrHeader {
			return false, 0, nil
		}
		// Terminator: classic EOF or OK-replacing-EOF (0xfe, len < 9).
		if row[0] == EOFHeader && len(row) < 9 {
			status = eofStatus(row, deprecateEOF)
			return statusHasMore(status), status, nil
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

// okStatus extracts SERVER_STATUS from a PROTOCOL_41 OK payload.
func okStatus(data []byte) uint16 {
	if len(data) < 1 || data[0] != OKHeader {
		return 0
	}
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
