package merge

import (
	"io"
	"sort"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/result"
)

// Sort buffers the upstream rows and sorts them by output columns. It is used
// after Group/Distinct, where per-shard k-way merging cannot provide a global
// order (the same group/distinct value may be produced by multiple shards).
func Sort(upstream result.Stream, fields []result.Field, keys []plan.OrderKey) (result.Stream, error) {
	if len(keys) == 0 {
		return upstream, nil
	}
	resolved := make([]resolvedKey, len(keys))
	for i, key := range keys {
		col := key.Column
		if key.Kind == plan.OrderByName {
			col = resolveField(fields, key.Name)
			if col < 0 {
				return nil, errs.Unsupported("ORDER BY column %q is missing from the result set", key.Name)
			}
		}
		if col < 0 || col >= len(fields) {
			return nil, errs.Unsupported("ORDER BY column %d is out of range", col)
		}
		kind, err := comparatorFor(fields[col])
		if err != nil {
			return nil, err
		}
		resolved[i] = resolvedKey{col: col, desc: key.Desc, kind: kind}
	}
	return &sortStream{upstream: upstream, fields: fields, keys: resolved}, nil
}

type sortStream struct {
	upstream result.Stream
	fields   []result.Field
	keys     []resolvedKey

	rows   []sortRow
	idx    int
	read   bool
	closed bool
}

type sortRow struct {
	row  result.Row
	vals []orderValue
}

func (s *sortStream) Fields() []result.Field { return s.fields }

func (s *sortStream) Next() (result.Row, error) {
	if !s.read {
		s.read = true
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	if s.idx >= len(s.rows) {
		return nil, io.EOF
	}
	row := s.rows[s.idx].row
	s.idx++
	return row, nil
}

func (s *sortStream) load() error {
	for {
		row, err := s.upstream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		vals, err := parseSortValues(row, s.keys)
		if err != nil {
			return err
		}
		s.rows = append(s.rows, sortRow{row: row, vals: vals})
	}
	sort.SliceStable(s.rows, func(i, j int) bool {
		return compareSortRows(s.rows[i], s.rows[j], s.keys) < 0
	})
	return nil
}

func parseSortValues(row result.Row, keys []resolvedKey) ([]orderValue, error) {
	vals := make([]orderValue, len(keys))
	for i, key := range keys {
		if key.col >= len(row) {
			return nil, errs.Unsupported("ORDER BY column %d exceeds row width %d", key.col, len(row))
		}
		v, err := parseOrderValue(row[key.col], key.kind)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	return vals, nil
}

func compareSortRows(a, b sortRow, keys []resolvedKey) int {
	for i, key := range keys {
		c := compareOrderValues(a.vals[i], b.vals[i], key.kind)
		if c == 0 {
			continue
		}
		if key.desc {
			return -c
		}
		return c
	}
	return 0
}

func (s *sortStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.upstream.Close()
}
