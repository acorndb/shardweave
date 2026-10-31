package packet

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// MaxPayloadLen is the maximum payload size of a single MySQL protocol frame.
const MaxPayloadLen = 1<<24 - 1 // 0xFFFFFF

// DefaultMaxAllowedPacket is the default logical packet limit (64 MiB, MySQL 8 default).
const DefaultMaxAllowedPacket = 64 << 20

// ErrPacketTooLarge is returned when a logical packet exceeds max_allowed_packet.
var ErrPacketTooLarge = errors.New("packet: payload exceeds max_allowed_packet")

// Conn handles MySQL packet framing. Shape mirrors go-mysql-org/go-mysql/packet.Conn
// for the uncompressed path: embed net.Conn, exported Sequence, WritePacket expects
// a buffer whose first 4 bytes are header space (payload starts at index 4).
type Conn struct {
	net.Conn
	Sequence uint8

	header           [4]byte
	readTimeout      time.Duration
	writeTimeout     time.Duration
	maxAllowedPacket int
}

// NewConn wraps c with MySQL packet framing.
func NewConn(c net.Conn) *Conn {
	return &Conn{Conn: c, maxAllowedPacket: DefaultMaxAllowedPacket}
}

// SetReadTimeout sets a per-read I/O deadline; zero disables it.
func (c *Conn) SetReadTimeout(d time.Duration) { c.readTimeout = d }

// SetWriteTimeout sets a per-write I/O deadline; zero disables it.
func (c *Conn) SetWriteTimeout(d time.Duration) { c.writeTimeout = d }

// SetMaxAllowedPacket sets the logical packet limit; <=0 restores the default.
func (c *Conn) SetMaxAllowedPacket(n int) {
	if n <= 0 {
		n = DefaultMaxAllowedPacket
	}
	c.maxAllowedPacket = n
}

// MaxAllowedPacket returns the logical packet limit.
func (c *Conn) MaxAllowedPacket() int { return c.maxAllowedPacket }

// ResetSequence sets Sequence to 0 (call before each new command after handshake).
func (c *Conn) ResetSequence() {
	c.Sequence = 0
}

// ReadPacket reads one logical MySQL packet payload (header stripped).
// Continuation frames (length == MaxPayloadLen) are accumulated into one payload,
// up to max_allowed_packet.
// Aligned with go-mysql packet.Conn.ReadPacket / ReadPacketTo (uncompressed).
func (c *Conn) ReadPacket() ([]byte, error) {
	var out []byte
	for {
		if c.readTimeout > 0 {
			_ = c.Conn.SetReadDeadline(time.Now().Add(c.readTimeout))
		}
		if _, err := io.ReadFull(c.Conn, c.header[:4]); err != nil {
			return nil, err
		}
		length := int(uint32(c.header[0]) | uint32(c.header[1])<<8 | uint32(c.header[2])<<16)
		seq := c.header[3]
		if seq != c.Sequence {
			return nil, fmt.Errorf("packet: invalid sequence %d != %d", seq, c.Sequence)
		}
		c.Sequence++

		if length > 0 {
			total := len(out) + length
			if c.maxAllowedPacket > 0 && total > c.maxAllowedPacket {
				return nil, fmt.Errorf("%w: %d > %d", ErrPacketTooLarge, total, c.maxAllowedPacket)
			}
			start := len(out)
			out = append(out, make([]byte, length)...)
			if c.readTimeout > 0 {
				_ = c.Conn.SetReadDeadline(time.Now().Add(c.readTimeout))
			}
			if _, err := io.ReadFull(c.Conn, out[start:]); err != nil {
				return nil, err
			}
		}
		if length < MaxPayloadLen {
			return out, nil
		}
	}
}

// WritePacket writes a packet. data already has a 4-byte header prefix; the
// payload is data[4:]. The header bytes are filled in-place (go-mysql convention).
func (c *Conn) WritePacket(data []byte) error {
	if len(data) < 4 {
		return fmt.Errorf("packet: WritePacket buffer too short: %d", len(data))
	}
	length := len(data) - 4
	if c.maxAllowedPacket > 0 && length > c.maxAllowedPacket {
		return fmt.Errorf("%w: %d > %d", ErrPacketTooLarge, length, c.maxAllowedPacket)
	}

	for length >= MaxPayloadLen {
		data[0] = 0xff
		data[1] = 0xff
		data[2] = 0xff
		data[3] = c.Sequence
		if c.writeTimeout > 0 {
			_ = c.Conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
		}
		if n, err := c.Conn.Write(data[:4+MaxPayloadLen]); err != nil {
			return err
		} else if n != 4+MaxPayloadLen {
			return io.ErrShortWrite
		}
		c.Sequence++
		length -= MaxPayloadLen
		data = data[MaxPayloadLen:]
	}

	data[0] = byte(length)
	data[1] = byte(length >> 8)
	data[2] = byte(length >> 16)
	data[3] = c.Sequence
	if c.writeTimeout > 0 {
		_ = c.Conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	}
	if n, err := c.Conn.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	c.Sequence++
	return nil
}

// Close resets sequence and closes the underlying connection.
func (c *Conn) Close() error {
	c.Sequence = 0
	if c.Conn != nil {
		return c.Conn.Close()
	}
	return nil
}
