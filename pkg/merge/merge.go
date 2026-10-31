// Package merge combines logical per-shard result streams.
// It does not import backend/execute/mysql: operators consume result.Stream
// and are pull-based, so callers control leasing and backpressure.
package merge

import (
	"errors"
	"io"

	"github.com/acorndb/shardweave/pkg/result"
)

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
