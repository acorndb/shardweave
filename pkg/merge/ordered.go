package merge

import (
	"bytes"
	"container/heap"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/result"
)

type valueKind uint8

const (
	kindInt valueKind = iota
	kindUint
	kindFloat
	kindDecimal
	kindText
)

type resolvedKey struct {
	col  int
	desc bool
	kind valueKind
}

// Ordered merges already-sorted shard streams with a k-way heap. Each stream
// must be sorted by the same keys (the shard executes the original ORDER BY).
// Columns are compared numerically/date-textually; string collation ordering
// is rejected instead of silently diverging from MySQL.
func Ordered(streams []result.Stream, fields []result.Field, keys []plan.OrderKey) (result.Stream, error) {
	kept := make([]result.Stream, 0, len(streams))
	for _, s := range streams {
		if s != nil {
			kept = append(kept, s)
		}
	}
	if len(keys) == 0 {
		return Concat(kept), nil
	}
	resolved := make([]resolvedKey, len(keys))
	for i, k := range keys {
		col := k.Column
		if k.Kind == plan.OrderByName {
			col = resolveField(fields, k.Name)
			if col < 0 {
				return nil, errs.Unsupported("ORDER BY column %q is missing from the result set", k.Name)
			}
		}
		if col >= len(fields) {
			return nil, errs.Unsupported("ORDER BY position %d is out of range", col+1)
		}
		kind, err := comparatorFor(fields[col])
		if err != nil {
			return nil, err
		}
		resolved[i] = resolvedKey{col: col, desc: k.Desc, kind: kind}
	}
	return &ordered{streams: kept, fields: fields, keys: resolved}, nil
}

func resolveField(fields []result.Field, name string) int {
	for i, f := range fields {
		if strings.EqualFold(f.Name, name) || strings.EqualFold(f.OrgName, name) {
			return i
		}
	}
	return -1
}

func comparatorFor(f result.Field) (valueKind, error) {
	switch f.Type {
	case result.TypeTiny, result.TypeShort, result.TypeLong, result.TypeLongLong, result.TypeInt24, result.TypeYear:
		if f.Flags&result.FlagUnsigned != 0 {
			return kindUint, nil
		}
		return kindInt, nil
	case result.TypeFloat, result.TypeDouble:
		return kindFloat, nil
	case result.TypeNewDecimal:
		return kindDecimal, nil
	case result.TypeTimestamp, result.TypeDate, result.TypeDateTime:
		return kindText, nil
	default:
		return 0, errs.Unsupported("multi-shard ORDER BY on column %q type 0x%02x is not supported",
			f.Name, f.Type)
	}
}

type orderValue struct {
	null bool
	i    int64
	u    uint64
	f    float64
	r    *big.Rat
	b    []byte
}

func parseOrderRow(row result.Row, keys []resolvedKey) ([]orderValue, error) {
	vals := make([]orderValue, len(keys))
	for i, k := range keys {
		if k.col >= len(row) {
			return nil, fmt.Errorf("merge: ORDER BY column %d out of row width %d", k.col, len(row))
		}
		v, err := parseOrderValue(row[k.col], k.kind)
		if err != nil {
			return nil, fmt.Errorf("merge: ORDER BY column %d: %w", k.col, err)
		}
		vals[i] = v
	}
	return vals, nil
}

func parseOrderValue(v result.Value, kind valueKind) (orderValue, error) {
	if v == nil {
		return orderValue{null: true}, nil
	}
	b, ok := v.([]byte)
	if !ok {
		return orderValue{}, fmt.Errorf("unsupported value type %T", v)
	}
	out := orderValue{}
	switch kind {
	case kindInt:
		n, err := strconv.ParseInt(string(b), 10, 64)
		if err != nil {
			return orderValue{}, fmt.Errorf("invalid integer %q", b)
		}
		out.i = n
	case kindUint:
		n, err := strconv.ParseUint(string(b), 10, 64)
		if err != nil {
			return orderValue{}, fmt.Errorf("invalid unsigned integer %q", b)
		}
		out.u = n
	case kindFloat:
		f, err := strconv.ParseFloat(string(b), 64)
		if err != nil {
			return orderValue{}, fmt.Errorf("invalid float %q", b)
		}
		out.f = f
	case kindDecimal:
		r, ok := new(big.Rat).SetString(string(b))
		if !ok {
			return orderValue{}, fmt.Errorf("invalid decimal %q", b)
		}
		out.r = r
	case kindText:
		out.b = b
	}
	return out, nil
}

func compareOrderValues(a, b orderValue, kind valueKind) int {
	if a.null || b.null {
		switch {
		case a.null && b.null:
			return 0
		case a.null:
			return -1 // MySQL sorts NULL first ascending
		default:
			return 1
		}
	}
	switch kind {
	case kindInt:
		return cmpInt64(a.i, b.i)
	case kindUint:
		switch {
		case a.u < b.u:
			return -1
		case a.u > b.u:
			return 1
		default:
			return 0
		}
	case kindFloat:
		switch {
		case a.f < b.f:
			return -1
		case a.f > b.f:
			return 1
		default:
			return 0
		}
	case kindDecimal:
		return a.r.Cmp(b.r)
	case kindText:
		return bytes.Compare(a.b, b.b)
	default:
		return 0
	}
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

type heapRow struct {
	idx  int
	row  result.Row
	vals []orderValue
}

type rowHeap struct {
	rows []*heapRow
	keys []resolvedKey
}

func (h rowHeap) Len() int { return len(h.rows) }

func (h rowHeap) Less(i, j int) bool {
	c := compareHeapRows(h.rows[i], h.rows[j], h.keys)
	if c != 0 {
		return c < 0
	}
	return h.rows[i].idx < h.rows[j].idx
}

func (h rowHeap) Swap(i, j int) { h.rows[i], h.rows[j] = h.rows[j], h.rows[i] }

func (h *rowHeap) Push(x any) { h.rows = append(h.rows, x.(*heapRow)) }

func (h *rowHeap) Pop() any {
	old := h.rows
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	h.rows = old[:n-1]
	return item
}

func compareHeapRows(a, b *heapRow, keys []resolvedKey) int {
	for i, k := range keys {
		c := compareOrderValues(a.vals[i], b.vals[i], k.kind)
		if c == 0 {
			continue
		}
		if k.desc {
			return -c
		}
		return c
	}
	return 0
}

type ordered struct {
	streams []result.Stream
	fields  []result.Field
	keys    []resolvedKey
	h       rowHeap

	started bool
	pending error
	closed  bool
}

func (o *ordered) Fields() []result.Field { return o.fields }

func (o *ordered) Next() (result.Row, error) {
	if o.pending != nil {
		return nil, o.pending
	}
	if !o.started {
		o.started = true
		o.h.keys = o.keys
		for i, s := range o.streams {
			if err := o.fill(i, s); err != nil {
				if errors.Is(err, io.EOF) {
					continue
				}
				o.pending = err
				return nil, err
			}
		}
	}
	if len(o.h.rows) == 0 {
		return nil, io.EOF
	}
	item := heap.Pop(&o.h).(*heapRow)
	if err := o.fill(item.idx, o.streams[item.idx]); err != nil && !errors.Is(err, io.EOF) {
		o.pending = err
	}
	return item.row, nil
}

func (o *ordered) fill(idx int, s result.Stream) error {
	row, err := s.Next()
	if err != nil {
		return err
	}
	vals, err := parseOrderRow(row, o.keys)
	if err != nil {
		return err
	}
	heap.Push(&o.h, &heapRow{idx: idx, row: row, vals: vals})
	return nil
}

func (o *ordered) Close() error {
	if o.closed {
		return nil
	}
	o.closed = true
	var first error
	for _, s := range o.streams {
		if err := s.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
