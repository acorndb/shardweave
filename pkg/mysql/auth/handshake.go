package auth

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/mysql/packet"
)

// Auth plugin names (MySQL / go-mysql).
const (
	NativePasswordPlugin      = "mysql_native_password"
	CachingSHA2PasswordPlugin = "caching_sha2_password"
)

// HandshakeV10 is Protocol::HandshakeV10 (server → client).
// Layout follows MySQL docs / go-mysql client.readInitialHandshake.
type HandshakeV10 struct {
	ProtocolVersion uint8
	ServerVersion   string
	ConnectionID    uint32
	AuthPluginData  []byte // scramble, typically 20 bytes
	Capability      uint32
	Collation       uint8
	Status          uint16
	AuthPluginName  string
}

// Encode serializes HandshakeV10 payload (without packet header).
func (h *HandshakeV10) Encode() ([]byte, error) {
	if h.ProtocolVersion == 0 {
		h.ProtocolVersion = 10
	}
	if len(h.AuthPluginData) < 8 {
		return nil, errors.New("auth: AuthPluginData must be at least 8 bytes")
	}
	data := make([]byte, 0, 128)
	data = append(data, h.ProtocolVersion)
	data = append(data, h.ServerVersion...)
	data = append(data, 0)
	var id [4]byte
	binary.LittleEndian.PutUint32(id[:], h.ConnectionID)
	data = append(data, id[:]...)
	data = append(data, h.AuthPluginData[:8]...)
	data = append(data, 0) // filler

	capLow := uint16(h.Capability)
	var tmp [2]byte
	binary.LittleEndian.PutUint16(tmp[:], capLow)
	data = append(data, tmp[:]...)

	data = append(data, h.Collation)
	binary.LittleEndian.PutUint16(tmp[:], h.Status)
	data = append(data, tmp[:]...)
	binary.LittleEndian.PutUint16(tmp[:], uint16(h.Capability>>16))
	data = append(data, tmp[:]...)

	authLen := byte(len(h.AuthPluginData))
	if authLen < 21 {
		authLen = 21 // MySQL pads scramble length field
	}
	data = append(data, authLen)
	data = append(data, make([]byte, 10)...) // reserved

	rest := h.AuthPluginData[8:]
	// rest written as 13 bytes (12 data + NUL) per protocol when secure connection
	part2 := make([]byte, 13)
	copy(part2, rest)
	data = append(data, part2...)

	if h.Capability&mysql.ClientPluginAuth != 0 {
		name := h.AuthPluginName
		if name == "" {
			name = NativePasswordPlugin
		}
		data = append(data, name...)
		data = append(data, 0)
	}
	return data, nil
}

// ParseHandshakeV10 parses a HandshakeV10 payload (go-mysql readInitialHandshake).
func ParseHandshakeV10(data []byte) (*HandshakeV10, error) {
	if len(data) < 1 {
		return nil, errors.New("auth: empty handshake")
	}
	if data[0] != 10 {
		return nil, fmt.Errorf("auth: unsupported protocol version %d", data[0])
	}
	h := &HandshakeV10{ProtocolVersion: data[0]}
	pos := 1

	end := bytes.IndexByte(data[pos:], 0)
	if end < 0 {
		return nil, errors.New("auth: malformed server version")
	}
	h.ServerVersion = string(data[pos : pos+end])
	pos += end + 1

	if pos+4 > len(data) {
		return nil, errors.New("auth: truncated connection id")
	}
	h.ConnectionID = binary.LittleEndian.Uint32(data[pos : pos+4])
	pos += 4

	if pos+8+1 > len(data) {
		return nil, errors.New("auth: truncated scramble part1")
	}
	h.AuthPluginData = append([]byte(nil), data[pos:pos+8]...)
	pos += 8
	if data[pos] != 0 {
		return nil, errors.New("auth: expected 0x00 after scramble part1")
	}
	pos++

	if pos+2 > len(data) {
		return nil, errors.New("auth: truncated capability low")
	}
	h.Capability = uint32(binary.LittleEndian.Uint16(data[pos : pos+2]))
	pos += 2
	if h.Capability&mysql.ClientProtocol41 == 0 {
		return nil, errors.New("auth: server lacks CLIENT_PROTOCOL_41")
	}

	if len(data) > pos {
		h.Collation = data[pos]
		pos++
		if pos+2 > len(data) {
			return nil, errors.New("auth: truncated status")
		}
		h.Status = binary.LittleEndian.Uint16(data[pos : pos+2])
		pos += 2
		if pos+2 > len(data) {
			return nil, errors.New("auth: truncated capability high")
		}
		h.Capability |= uint32(binary.LittleEndian.Uint16(data[pos:pos+2])) << 16
		pos += 2

		authPluginDataLen := data[pos]
		pos++
		pos += 10 // reserved

		if h.Capability&mysql.ClientSecureConnection != 0 {
			rest := max(int(authPluginDataLen)-8, 13)
			if pos+rest > len(data) {
				return nil, errors.New("auth: truncated scramble part2")
			}
			part2 := data[pos : pos+rest-1] // exclude trailing NUL
			pos += rest
			h.AuthPluginData = append(h.AuthPluginData, part2...)
		}

		if h.Capability&mysql.ClientPluginAuth != 0 {
			if pos >= len(data) {
				return nil, errors.New("auth: truncated auth plugin name")
			}
			zend := bytes.IndexByte(data[pos:], 0)
			if zend < 0 {
				return nil, errors.New("auth: malformed auth plugin name")
			}
			h.AuthPluginName = string(data[pos : pos+zend])
		}
	}
	if h.AuthPluginName == "" {
		h.AuthPluginName = NativePasswordPlugin
	}
	return h, nil
}

// HandshakeResponse41 is Protocol::HandshakeResponse41 (client → server).
type HandshakeResponse41 struct {
	Capability     uint32
	MaxPacketSize  uint32
	Collation      uint8
	Username       string
	AuthResponse   []byte
	Database       string
	AuthPluginName string
}

// Encode serializes HandshakeResponse41 payload (no packet header).
// Callers writing via packet.Conn should prepend 4 header bytes (go-mysql convention).
func (r *HandshakeResponse41) Encode() ([]byte, error) {
	capability := r.Capability
	capability |= mysql.ClientProtocol41 | mysql.ClientSecureConnection | mysql.ClientPluginAuth
	if r.Database == "" {
		capability &^= mysql.ClientConnectWithDB
	} else {
		capability |= mysql.ClientConnectWithDB
	}

	auth := r.AuthResponse
	authLEI := packet.AppendLenEncInt(nil, uint64(len(auth)))
	if len(authLEI) > 1 {
		capability |= mysql.ClientPluginAuthLenEncClientData
	}

	plugin := r.AuthPluginName
	if plugin == "" {
		plugin = NativePasswordPlugin
	}

	length := 4 + 4 + 1 + 23 + len(r.Username) + 1 + len(authLEI) + len(auth) + len(plugin) + 1
	if capability&mysql.ClientConnectWithDB != 0 {
		length += len(r.Database) + 1
	}

	data := make([]byte, length)
	data[0] = byte(capability)
	data[1] = byte(capability >> 8)
	data[2] = byte(capability >> 16)
	data[3] = byte(capability >> 24)
	data[8] = r.Collation
	pos := 9 + 23

	pos += copy(data[pos:], r.Username)
	data[pos] = 0
	pos++

	pos += copy(data[pos:], authLEI)
	pos += copy(data[pos:], auth)

	if capability&mysql.ClientConnectWithDB != 0 {
		pos += copy(data[pos:], r.Database)
		data[pos] = 0
		pos++
	}

	pos += copy(data[pos:], plugin)
	data[pos] = 0
	pos++

	r.Capability = capability
	return data[:pos], nil
}

// ParseHandshakeResponse41 parses client response payload (no packet header).
func ParseHandshakeResponse41(data []byte, _ uint32) (*HandshakeResponse41, error) {
	if len(data) < 4+4+1+23 {
		return nil, errors.New("auth: handshake response too short")
	}
	r := &HandshakeResponse41{}
	r.Capability = binary.LittleEndian.Uint32(data[0:4])
	r.MaxPacketSize = binary.LittleEndian.Uint32(data[4:8])
	r.Collation = data[8]
	pos := 9 + 23

	if pos >= len(data) {
		return nil, errors.New("auth: truncated username")
	}
	zend := bytes.IndexByte(data[pos:], 0)
	if zend < 0 {
		return nil, errors.New("auth: malformed username")
	}
	r.Username = string(data[pos : pos+zend])
	pos += zend + 1

	if r.Capability&mysql.ClientPluginAuthLenEncClientData != 0 {
		n, adv, isNull, err := packet.ReadLenEncInt(data[pos:])
		if err != nil {
			return nil, err
		}
		if isNull {
			return nil, errors.New("auth: unexpected NULL auth data")
		}
		pos += adv
		if pos+int(n) > len(data) {
			return nil, errors.New("auth: truncated auth response")
		}
		r.AuthResponse = append([]byte(nil), data[pos:pos+int(n)]...)
		pos += int(n)
	} else if r.Capability&mysql.ClientSecureConnection != 0 {
		if pos >= len(data) {
			return nil, errors.New("auth: truncated auth length")
		}
		n := int(data[pos])
		pos++
		if pos+n > len(data) {
			return nil, errors.New("auth: truncated auth response")
		}
		r.AuthResponse = append([]byte(nil), data[pos:pos+n]...)
		pos += n
	} else {
		zend = bytes.IndexByte(data[pos:], 0)
		if zend < 0 {
			return nil, errors.New("auth: malformed auth response")
		}
		r.AuthResponse = append([]byte(nil), data[pos:pos+zend]...)
		pos += zend + 1
	}

	if r.Capability&mysql.ClientConnectWithDB != 0 {
		if pos >= len(data) {
			return nil, errors.New("auth: truncated database")
		}
		zend = bytes.IndexByte(data[pos:], 0)
		if zend < 0 {
			return nil, errors.New("auth: malformed database")
		}
		r.Database = string(data[pos : pos+zend])
		pos += zend + 1
	}

	if r.Capability&mysql.ClientPluginAuth != 0 && pos < len(data) {
		zend = bytes.IndexByte(data[pos:], 0)
		if zend < 0 {
			r.AuthPluginName = string(data[pos:])
		} else {
			r.AuthPluginName = string(data[pos : pos+zend])
		}
	}
	if r.AuthPluginName == "" {
		r.AuthPluginName = NativePasswordPlugin
	}
	return r, nil
}
