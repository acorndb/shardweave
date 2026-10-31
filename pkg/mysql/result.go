package mysql

import (
	"encoding/binary"
	"fmt"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
)

// EncodeOK builds a minimal OK_Packet payload (no packet header).
// Compatible with go-mysql / MySQL OK packet for capability PROTOCOL_41.
func EncodeOK(affectedRows, lastInsertID uint64, status, warnings uint16) []byte {
	data := []byte{OKHeader}
	data = appendLenEnc(data, affectedRows)
	data = appendLenEnc(data, lastInsertID)
	var tmp [2]byte
	binary.LittleEndian.PutUint16(tmp[:], status)
	data = append(data, tmp[:]...)
	binary.LittleEndian.PutUint16(tmp[:], warnings)
	data = append(data, tmp[:]...)
	return data
}

// EncodeERR builds ERR_Packet payload (no packet header).
func EncodeERR(errno uint16, sqlState, message string) []byte {
	if sqlState == "" {
		sqlState = "HY000"
	}
	if len(sqlState) != 5 {
		sqlState = "HY000"
	}
	data := make([]byte, 0, 9+len(message))
	data = append(data, ErrHeader)
	var tmp [2]byte
	binary.LittleEndian.PutUint16(tmp[:], errno)
	data = append(data, tmp[:]...)
	data = append(data, '#')
	data = append(data, sqlState...)
	data = append(data, message...)
	return data
}

// WriteERR writes one complete ERR packet to front. It owns the 4-byte header
// framing so proxy/server do not each duplicate the packet construction.
func WriteERR(front *packet.Conn, e *errs.MySQL) error {
	if front == nil {
		return fmt.Errorf("mysql: nil frontend for WriteERR")
	}
	if e == nil {
		e = errs.Unknown("unknown error")
	}
	payload := EncodeERR(e.Number, e.SQLState, e.Message)
	buf := make([]byte, 4+len(payload))
	copy(buf[4:], payload)
	return front.WritePacket(buf)
}

// Column is one protocol-level ColumnDefinition41.
type Column struct {
	Schema       string
	Table        string
	OrgTable     string
	Name         string
	OrgName      string
	Charset      uint16
	ColumnLength uint32
	Type         byte
	Flags        uint16
	Decimals     byte
}

// EncodeColumnDefinition builds a ColumnDefinition41 payload (no packet header).
func EncodeColumnDefinition(c Column) []byte {
	var out []byte
	out = appendLenEncString(out, "def")
	out = appendLenEncString(out, c.Schema)
	out = appendLenEncString(out, c.Table)
	out = appendLenEncString(out, c.OrgTable)
	out = appendLenEncString(out, c.Name)
	out = appendLenEncString(out, c.OrgName)
	out = appendLenEnc(out, 0x0c)

	var tmp [4]byte
	binary.LittleEndian.PutUint16(tmp[:2], c.Charset)
	out = append(out, tmp[:2]...)
	binary.LittleEndian.PutUint32(tmp[:], c.ColumnLength)
	out = append(out, tmp[:]...)
	out = append(out, c.Type)
	binary.LittleEndian.PutUint16(tmp[:2], c.Flags)
	out = append(out, tmp[:2]...)
	out = append(out, c.Decimals, 0x00, 0x00)
	return out
}

// EncodeColumnCount is the first packet of a text result set.
func EncodeColumnCount(n uint64) []byte {
	return appendLenEnc(nil, n)
}

// EncodeTextRow encodes one text-protocol row. A nil cell is SQL NULL; an
// empty non-nil cell is the empty string.
func EncodeTextRow(cells [][]byte) []byte {
	var out []byte
	for _, cell := range cells {
		if cell == nil {
			out = append(out, 0xfb)
			continue
		}
		out = appendLenEnc(out, uint64(len(cell)))
		out = append(out, cell...)
	}
	return out
}

// EncodeEOF builds the result-set terminator. With CLIENT_DEPRECATE_EOF the
// server sends an OK packet whose header is 0xfe; otherwise it sends the
// classic EOF packet (warnings, status).
func EncodeEOF(deprecateEOF bool, status, warnings uint16) []byte {
	if deprecateEOF {
		p := EncodeOK(0, 0, status, warnings)
		p[0] = EOFHeader
		return p
	}
	out := []byte{EOFHeader}
	var tmp [2]byte
	binary.LittleEndian.PutUint16(tmp[:], warnings)
	out = append(out, tmp[:]...)
	binary.LittleEndian.PutUint16(tmp[:], status)
	out = append(out, tmp[:]...)
	return out
}

func appendLenEncString(b []byte, s string) []byte {
	b = appendLenEnc(b, uint64(len(s)))
	return append(b, s...)
}

func appendLenEnc(b []byte, n uint64) []byte {
	switch {
	case n < 251:
		return append(b, byte(n))
	case n <= 0xffff:
		return append(b, 0xfc, byte(n), byte(n>>8))
	case n <= 0xffffff:
		return append(b, 0xfd, byte(n), byte(n>>8), byte(n>>16))
	default:
		return append(b, 0xfe,
			byte(n), byte(n>>8), byte(n>>16), byte(n>>24),
			byte(n>>32), byte(n>>40), byte(n>>48), byte(n>>56))
	}
}
