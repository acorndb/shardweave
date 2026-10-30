package mysql

import (
	"encoding/binary"
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
