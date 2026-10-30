package packet

import (
	"encoding/binary"
	"errors"
	"fmt"
)

var (
	errLenEncTruncated = errors.New("packet: truncated length-encoded value")
	errLenEncInvalid   = errors.New("packet: invalid length-encoded integer")
)

// AppendLenEncInt appends a MySQL length-encoded integer to b.
// Values 0–250 use one byte; 0xfb is reserved for NULL on the wire and is
// never emitted here (251 is encoded with the 0xfc two-byte form).
func AppendLenEncInt(b []byte, n uint64) []byte {
	switch {
	case n < 251:
		return append(b, byte(n))
	case n <= 0xffff:
		b = append(b, 0xfc, 0, 0)
		binary.LittleEndian.PutUint16(b[len(b)-2:], uint16(n))
		return b
	case n <= 0xffffff:
		b = append(b, 0xfd, 0, 0, 0)
		b[len(b)-3] = byte(n)
		b[len(b)-2] = byte(n >> 8)
		b[len(b)-1] = byte(n >> 16)
		return b
	default:
		b = append(b, 0xfe, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.LittleEndian.PutUint64(b[len(b)-8:], n)
		return b
	}
}

// ReadLenEncInt reads a length-encoded integer from b.
// isNull is true when the first byte is 0xfb (SQL NULL marker).
func ReadLenEncInt(b []byte) (n uint64, advance int, isNull bool, err error) {
	if len(b) == 0 {
		return 0, 0, false, errLenEncTruncated
	}
	switch b[0] {
	case 0xfb:
		return 0, 1, true, nil
	case 0xfc:
		if len(b) < 3 {
			return 0, 0, false, errLenEncTruncated
		}
		return uint64(binary.LittleEndian.Uint16(b[1:3])), 3, false, nil
	case 0xfd:
		if len(b) < 4 {
			return 0, 0, false, errLenEncTruncated
		}
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16, 4, false, nil
	case 0xfe:
		if len(b) < 9 {
			return 0, 0, false, errLenEncTruncated
		}
		return binary.LittleEndian.Uint64(b[1:9]), 9, false, nil
	default:
		if b[0] >= 251 {
			return 0, 0, false, fmt.Errorf("%w: 0x%02x", errLenEncInvalid, b[0])
		}
		return uint64(b[0]), 1, false, nil
	}
}

// AppendLenEncString appends a length-encoded string to b.
func AppendLenEncString(b []byte, s string) []byte {
	b = AppendLenEncInt(b, uint64(len(s)))
	return append(b, s...)
}

// ReadLenEncString reads a length-encoded string from b.
// A NULL marker (0xfb) is returned as an error — callers that need SQL NULL
// should use ReadLenEncInt first.
func ReadLenEncString(b []byte) (s string, advance int, err error) {
	n, adv, isNull, err := ReadLenEncInt(b)
	if err != nil {
		return "", 0, err
	}
	if isNull {
		return "", 0, errors.New("packet: unexpected NULL length-encoded string")
	}
	end := adv + int(n)
	if n > uint64(len(b)-adv) || end < adv {
		return "", 0, errLenEncTruncated
	}
	return string(b[adv:end]), end, nil
}
