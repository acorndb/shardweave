package mysql

import (
	"encoding/binary"
	"fmt"

	"github.com/acorndb/shardweave/pkg/errs"
)

// DecodeLenEncInt exposes the length-encoded integer decoder for higher layers.
func DecodeLenEncInt(data []byte) (uint64, int, error) {
	return readLenEncInt(data)
}

// DecodeColumn parses a ColumnDefinition41 payload.
func DecodeColumn(data []byte) (Column, error) {
	var c Column
	pos := 0
	readString := func() (string, error) {
		n, adv, err := readLenEncInt(data[pos:])
		if err != nil {
			return "", err
		}
		pos += adv
		if n > uint64(len(data)-pos) {
			return "", fmt.Errorf("mysql: truncated column string")
		}
		s := string(data[pos : pos+int(n)])
		pos += int(n)
		return s, nil
	}

	var err error
	if _, err = readString(); err != nil { // catalog
		return Column{}, err
	}
	if c.Schema, err = readString(); err != nil {
		return Column{}, err
	}
	if c.Table, err = readString(); err != nil {
		return Column{}, err
	}
	if c.OrgTable, err = readString(); err != nil {
		return Column{}, err
	}
	if c.Name, err = readString(); err != nil {
		return Column{}, err
	}
	if c.OrgName, err = readString(); err != nil {
		return Column{}, err
	}

	fixed, adv, err := readLenEncInt(data[pos:])
	if err != nil {
		return Column{}, err
	}
	pos += adv
	if fixed != 0x0c {
		return Column{}, fmt.Errorf("mysql: unsupported column fixed length %d", fixed)
	}
	if len(data)-pos < 12 {
		return Column{}, fmt.Errorf("mysql: truncated column definition")
	}
	c.Charset = binary.LittleEndian.Uint16(data[pos:])
	pos += 2
	c.ColumnLength = binary.LittleEndian.Uint32(data[pos:])
	pos += 4
	c.Type = data[pos]
	pos++
	c.Flags = binary.LittleEndian.Uint16(data[pos:])
	pos += 2
	c.Decimals = data[pos]
	pos++
	pos += 2 // filler
	if pos != len(data) {
		return Column{}, fmt.Errorf("mysql: trailing column bytes")
	}
	return c, nil
}

// DecodeTextRow parses one text-protocol row into n cells. A nil cell is NULL.
func DecodeTextRow(data []byte, n int) ([][]byte, error) {
	cells := make([][]byte, 0, n)
	pos := 0
	for len(cells) < n {
		if pos >= len(data) {
			return nil, fmt.Errorf("mysql: truncated row")
		}
		if data[pos] == 0xfb {
			cells = append(cells, nil)
			pos++
			continue
		}
		l, adv, err := readLenEncInt(data[pos:])
		if err != nil {
			return nil, err
		}
		pos += adv
		if l > uint64(len(data)-pos) {
			return nil, fmt.Errorf("mysql: truncated row value")
		}
		cell := make([]byte, int(l))
		copy(cell, data[pos:pos+int(l)])
		cells = append(cells, cell)
		pos += int(l)
	}
	if pos != len(data) {
		return nil, fmt.Errorf("mysql: trailing row bytes")
	}
	return cells, nil
}

// DecodeServerError parses an ERR packet.
func DecodeServerError(data []byte) error {
	if len(data) < 3 || data[0] != ErrHeader {
		return fmt.Errorf("mysql: not an ERR packet")
	}
	e := &errs.MySQL{
		Number:   binary.LittleEndian.Uint16(data[1:3]),
		SQLState: "HY000",
	}
	pos := 3
	if pos < len(data) && data[pos] == '#' && len(data) >= pos+6 {
		e.SQLState = string(data[pos+1 : pos+6])
		pos += 6
	}
	e.Message = string(data[pos:])
	return e
}
