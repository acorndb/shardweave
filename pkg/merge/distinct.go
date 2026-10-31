package merge

import (
	"io"

	"github.com/acorndb/shardweave/pkg/result"
)

// Distinct deduplicates rows across shards. Per-shard SQL already applies
// DISTINCT, so only cross-shard duplicates remain. Numeric/date keys are
// canonicalized; string collations are rejected until weights are available.
func Distinct(streams []result.Stream, fields []result.Field) (result.Stream, error) {
	kinds := make([]valueKind, len(fields))
	for i, field := range fields {
		kind, err := comparatorFor(field)
		if err != nil {
			return nil, err
		}
		kinds[i] = kind
	}
	cols := make([]int, len(fields))
	for i := range cols {
		cols[i] = i
	}
	kept := make([]result.Stream, 0, len(streams))
	for _, s := range streams {
		if s != nil {
			kept = append(kept, s)
		}
	}
	return &distinctStream{
		streams: kept,
		fields:  fields,
		cols:    cols,
		kinds:   kinds,
		seen:    make(map[string]struct{}),
	}, nil
}

type distinctStream struct {
	streams []result.Stream
	fields  []result.Field
	cols    []int
	kinds   []valueKind

	seen   map[string]struct{}
	rows   []result.Row
	idx    int
	read   bool
	closed bool
}

func (d *distinctStream) Fields() []result.Field { return d.fields }

func (d *distinctStream) Next() (result.Row, error) {
	if !d.read {
		d.read = true
		if err := d.fold(); err != nil {
			return nil, err
		}
	}
	if d.idx >= len(d.rows) {
		return nil, io.EOF
	}
	row := d.rows[d.idx]
	d.idx++
	return row, nil
}

func (d *distinctStream) fold() error {
	for _, stream := range d.streams {
		for {
			row, err := stream.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			key, err := encodeRowKey(row, d.cols, d.kinds)
			if err != nil {
				return err
			}
			if _, ok := d.seen[key]; ok {
				continue
			}
			d.seen[key] = struct{}{}
			d.rows = append(d.rows, copyRow(row))
		}
	}
	return nil
}

func (d *distinctStream) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	var first error
	for _, s := range d.streams {
		if err := s.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func copyRow(row result.Row) result.Row {
	out := make(result.Row, len(row))
	for i, v := range row {
		out[i] = copyValue(v)
	}
	return out
}
