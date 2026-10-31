// Package merge combines logical per-shard result streams.
// It does not import backend/execute/mysql: operators consume result.Stream
// and are pull-based, so callers control leasing and backpressure.
package merge

import (
	"errors"
	"io"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/result"
)

// Validate checks merge features independently of streams/fields, so execute
// can reject unsupported plans before acquiring backends.
func Validate(spec plan.MergeSpec) error {
	if len(spec.GroupBy) > 0 || spec.Distinct || len(spec.Output) > 0 {
		return errs.Unsupported("multi-shard merge shape is not supported")
	}
	if len(spec.Aggregates) > 0 && len(spec.OrderBy) > 0 {
		return errs.Unsupported("multi-shard aggregate with ORDER BY is not supported")
	}
	return nil
}

// Merge builds the operator tree for a MultiShard plan. Execute only supplies
// streams/fields/spec; operator selection stays in this package.
func Merge(streams []result.Stream, fields []result.Field, spec plan.MergeSpec) (result.Stream, error) {
	if err := Validate(spec); err != nil {
		return nil, err
	}
	var out result.Stream
	switch {
	case len(spec.Aggregates) > 0:
		agg, err := Aggregate(streams, fields, spec.Aggregates)
		if err != nil {
			return nil, err
		}
		out = agg
	case len(spec.OrderBy) > 0:
		ordered, err := Ordered(streams, fields, spec.OrderBy)
		if err != nil {
			return nil, err
		}
		out = ordered
	default:
		out = Concat(streams)
	}
	if spec.Limit != nil {
		out = Limit(out, spec.Limit.Offset, spec.Limit.Count)
	}
	return out, nil
}

// Concat streams the shard results in order, skipping nil streams.
// It is the identity merge for multi-shard queries without ORDER BY.
func Concat(streams []result.Stream) result.Stream {
	kept := make([]result.Stream, 0, len(streams))
	for _, s := range streams {
		if s != nil {
			kept = append(kept, s)
		}
	}
	return &concat{streams: kept}
}

type concat struct {
	streams []result.Stream
	idx     int
	closed  bool
}

func (c *concat) Fields() []result.Field {
	for _, s := range c.streams {
		if f := s.Fields(); len(f) > 0 {
			return f
		}
	}
	return nil
}

func (c *concat) Next() (result.Row, error) {
	for c.idx < len(c.streams) {
		row, err := c.streams[c.idx].Next()
		if errors.Is(err, io.EOF) {
			c.idx++
			continue
		}
		return row, err
	}
	return nil, io.EOF
}

func (c *concat) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	var first error
	for _, s := range c.streams {
		if err := s.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Limit applies OFFSET/COUNT to one upstream stream. It stops reading once
// COUNT rows have been produced; callers that stop early must close and
// discard the underlying backend streams.
func Limit(upstream result.Stream, offset, count int64) result.Stream {
	if offset < 0 {
		offset = 0
	}
	return &limit{upstream: upstream, offset: offset, remaining: count}
}

type limit struct {
	upstream  result.Stream
	offset    int64
	remaining int64
	closed    bool
}

func (l *limit) Fields() []result.Field { return l.upstream.Fields() }

func (l *limit) Next() (result.Row, error) {
	for l.offset > 0 {
		if _, err := l.upstream.Next(); err != nil {
			return nil, err
		}
		l.offset--
	}
	if l.remaining <= 0 {
		return nil, io.EOF
	}
	row, err := l.upstream.Next()
	if err != nil {
		return nil, err
	}
	l.remaining--
	return row, nil
}

func (l *limit) Close() error {
	if l.closed {
		return nil
	}
	l.closed = true
	return l.upstream.Close()
}
