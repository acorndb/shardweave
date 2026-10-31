package backend

import (
	"fmt"
	"io"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/acorndb/shardweave/pkg/result"
)

type decodedStream struct {
	pc           *packet.Conn
	deprecateEOF bool
	fields       []result.Field
	ncols        int
	done         bool
}

// Decode switches a raw stream into logical rows. It must be called before
// ForwardTo; the two consume the same packet stream.
func (s *rawStream) Decode() (result.Stream, error) {
	if s.prepare {
		return nil, fmt.Errorf("backend: COM_STMT_PREPARE response is not a result set")
	}
	return newDecodedStream(s.pc, true)
}

func newDecodedStream(pc *packet.Conn, deprecateEOF bool) (*decodedStream, error) {
	data, err := pc.ReadPacket()
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("backend: empty response")
	}
	switch data[0] {
	case mysql.OKHeader:
		return nil, errs.ErrNoResultSet
	case mysql.ErrHeader:
		return nil, mysql.DecodeServerError(data)
	}

	ncols, adv, err := mysql.DecodeLenEncInt(data)
	if err != nil {
		return nil, err
	}
	if adv != len(data) {
		return nil, fmt.Errorf("backend: unexpected bytes in column-count packet")
	}
	if ncols == 0 {
		return nil, fmt.Errorf("backend: result set has no columns")
	}

	s := &decodedStream{pc: pc, deprecateEOF: deprecateEOF, ncols: int(ncols)}
	s.fields = make([]result.Field, 0, s.ncols)
	for i := 0; i < s.ncols; i++ {
		def, err := pc.ReadPacket()
		if err != nil {
			return nil, err
		}
		col, err := mysql.DecodeColumn(def)
		if err != nil {
			return nil, err
		}
		s.fields = append(s.fields, result.Field{
			Name:         col.Name,
			OrgName:      col.OrgName,
			Table:        col.Table,
			OrgTable:     col.OrgTable,
			DB:           col.Schema,
			Type:         col.Type,
			Flags:        col.Flags,
			Charset:      col.Charset,
			ColumnLength: col.ColumnLength,
			Decimals:     col.Decimals,
		})
	}
	if !deprecateEOF {
		mark, err := pc.ReadPacket()
		if err != nil {
			return nil, err
		}
		if len(mark) == 0 || mark[0] != mysql.EOFHeader {
			return nil, fmt.Errorf("backend: expected EOF after columns")
		}
	}
	return s, nil
}

func (s *decodedStream) Fields() []result.Field { return s.fields }

func (s *decodedStream) Next() (result.Row, error) {
	if s.done {
		return nil, io.EOF
	}
	for {
		data, err := s.pc.ReadPacket()
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			continue
		}
		if data[0] == mysql.ErrHeader {
			return nil, mysql.DecodeServerError(data)
		}
		if data[0] == mysql.EOFHeader && len(data) < 9 {
			s.done = true
			return nil, io.EOF
		}
		cells, err := mysql.DecodeTextRow(data, s.ncols)
		if err != nil {
			return nil, err
		}
		row := make(result.Row, len(cells))
		for i, c := range cells {
			if c != nil {
				row[i] = c
			}
		}
		return row, nil
	}
}

func (s *decodedStream) Close() error {
	s.done = true
	return nil
}
