package execute

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/merge"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/result"
	"github.com/acorndb/shardweave/pkg/route"
	"github.com/acorndb/shardweave/pkg/sql"
)

// decodedStreamer is implemented by backend streams that expose logical rows
// instead of raw packet forwarding.
type decodedStreamer interface {
	Decode() (result.Stream, error)
}

// dispatch runs the multi-shard query path: scatter the same SQL to every
// target, execute headers concurrently, verify column metadata, then hand the
// streams to the merge engine.
func (p *Pipeline) dispatch(ctx context.Context, rt *Runtime, stmt *sql.Statement, pl *plan.Plan) (bool, error) {
	if stmt.Cmd != mysql.ComQuery {
		return false, rejectMulti("multi-shard command %d is not supported", stmt.Cmd)
	}
	if len(pl.Targets) == 0 {
		return false, fmt.Errorf("execute: multi-shard plan has no targets")
	}
	if err := merge.Validate(pl.Merge); err != nil {
		return false, err
	}
	if rt.Front == nil {
		return false, fmt.Errorf("execute: multi-shard plan requires a frontend")
	}

	type shardResult struct {
		slice  string
		stream result.Stream
	}
	opened := make([]shardResult, 0, len(pl.Targets))
	cleanup := func(discard bool) {
		for _, o := range opened {
			_ = o.stream.Close()
		}
		opened = opened[:0]
		for _, t := range pl.Targets {
			_ = p.releaseOne(rt, t.Slice, discard)
		}
	}

	// Acquire leases sequentially (the lease map is session state), then run
	// align/exec/header decoding concurrently across independent shard conns.
	leases := make([]*lease, len(pl.Targets))
	for i, target := range pl.Targets {
		l, _, err := p.acquireLease(ctx, rt, target)
		if err != nil {
			cleanup(true)
			return false, err
		}
		leases[i] = l
	}

	type resultSlot struct {
		stream result.Stream
		err    error
	}
	results := make([]resultSlot, len(pl.Targets))
	var wg sync.WaitGroup
	for i, target := range pl.Targets {
		wg.Add(1)
		go func(i int, target route.Target) {
			defer wg.Done()
			stream, err := p.openLeaseDecoded(ctx, rt, stmt, target, leases[i])
			results[i] = resultSlot{stream: stream, err: err}
		}(i, target)
	}
	wg.Wait()

	for i, res := range results {
		if res.err != nil {
			for _, r := range results {
				if r.stream != nil {
					_ = r.stream.Close()
				}
			}
			cleanup(true)
			var se *errs.MySQL
			if errors.As(res.err, &se) {
				// Nothing has been written yet; forward the backend ERR and keep
				// the session alive, matching the single-target raw path.
				return false, mysql.WriteERR(rt.Front, se)
			}
			return false, res.err
		}
		opened = append(opened, shardResult{slice: pl.Targets[i].Slice, stream: res.stream})
	}

	firstSlice := opened[0].slice
	fields := opened[0].stream.Fields()
	if len(fields) == 0 {
		cleanup(true)
		return false, fmt.Errorf("execute: shard %s returned no columns", firstSlice)
	}
	for _, o := range opened[1:] {
		if !sameFields(fields, o.stream.Fields()) {
			cleanup(true)
			return false, fmt.Errorf("execute: shard %s column metadata mismatch: %+v != %+v",
				o.slice, o.stream.Fields(), fields)
		}
	}

	streams := make([]result.Stream, 0, len(opened))
	for _, o := range opened {
		streams = append(streams, o.stream)
	}
	merged, mErr := merge.Merge(streams, fields, pl.Merge)
	if mErr != nil {
		cleanup(true)
		return false, mErr
	}
	outFields := merged.Fields()
	if len(outFields) == 0 {
		cleanup(true)
		return false, fmt.Errorf("execute: merge returned no output columns")
	}

	// All headers, ORDER BY comparators and aggregate functions are valid, so
	// a partial result set is no longer possible.
	if err := mysql.WriteResultHeader(rt.Front, resultColumns(outFields)); err != nil {
		cleanup(true)
		return false, err
	}
	for {
		row, err := merged.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup(true)
			return false, fmt.Errorf("execute: merge: %w", err)
		}
		cells, err := encodeCells(row, len(outFields))
		if err != nil {
			cleanup(true)
			return false, err
		}
		if err := writeFrontPayload(rt.Front, mysql.EncodeTextRow(cells)); err != nil {
			cleanup(true)
			return false, err
		}
	}
	if err := mysql.WriteEOF(rt.Front, frontendDeprecateEOF, mysql.ServerStatusAutocommit, 0); err != nil {
		cleanup(true)
		return false, err
	}

	// With LIMIT the merge may stop before every shard stream reaches EOF;
	// unread packets make the connection unsafe to reuse, so discard.
	cleanup(pl.Merge.Limit != nil)
	rt.State.SetRowCount(-1)
	return true, nil
}

// openDecoded acquires/reuses the slice lease, aligns it, executes the payload
// and reads the result header. Rows stay unread for the caller to stream.
func (p *Pipeline) openDecoded(ctx context.Context, rt *Runtime, stmt *sql.Statement, target route.Target) (result.Stream, error) {
	stream, err := p.execTarget(ctx, rt, stmt, target)
	if err != nil {
		return nil, err
	}
	d, ok := stream.(decodedStreamer)
	if !ok {
		_ = stream.Close()
		return nil, fmt.Errorf("execute: stream %T cannot decode result rows", stream)
	}
	decoded, err := d.Decode()
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	return decoded, nil
}

func (p *Pipeline) openLeaseDecoded(
	ctx context.Context,
	rt *Runtime,
	stmt *sql.Statement,
	target route.Target,
	l *lease,
) (result.Stream, error) {
	stream, err := p.execOnLease(ctx, rt, stmt, target, l)
	if err != nil {
		return nil, err
	}
	d, ok := stream.(decodedStreamer)
	if !ok {
		_ = stream.Close()
		return nil, fmt.Errorf("execute: stream %T cannot decode result rows", stream)
	}
	decoded, err := d.Decode()
	if err != nil {
		_ = stream.Close()
		return nil, err
	}
	return decoded, nil
}

func sameFields(a, b []result.Field) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func encodeCells(row result.Row, n int) ([][]byte, error) {
	if len(row) != n {
		return nil, fmt.Errorf("execute: row has %d cells, want %d", len(row), n)
	}
	cells := make([][]byte, n)
	for i, v := range row {
		switch x := v.(type) {
		case nil:
			cells[i] = nil
		case []byte:
			cells[i] = x
		case string:
			cells[i] = []byte(x)
		default:
			return nil, fmt.Errorf("execute: unsupported result value type %T", v)
		}
	}
	return cells, nil
}

// resultColumns converts logical fields to protocol column definitions.
func resultColumns(fields []result.Field) []mysql.Column {
	out := make([]mysql.Column, 0, len(fields))
	for _, f := range fields {
		out = append(out, mysql.Column{
			Schema:       f.DB,
			Table:        f.Table,
			OrgTable:     f.OrgTable,
			Name:         f.Name,
			OrgName:      f.OrgName,
			Charset:      f.Charset,
			ColumnLength: f.ColumnLength,
			Type:         f.Type,
			Flags:        f.Flags,
			Decimals:     f.Decimals,
		})
	}
	return out
}

func rejectMulti(format string, args ...any) error {
	return errs.Unsupported(format, args...)
}
