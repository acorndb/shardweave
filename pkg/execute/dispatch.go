package execute

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/acorndb/shardweave/pkg/backend"
	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/merge"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
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

// dispatch runs the P0 multi-shard query path: scatter the same SQL to every
// target, verify the column metadata, then concatenate rows in target order.
// The router only produces multi-target SELECT plans that need no ORDER BY,
// LIMIT, DISTINCT or aggregate merge; D5/D6 replace this with the merge engine.
func (p *Pipeline) dispatch(ctx context.Context, rt *Runtime, stmt *sql.Statement, pl *plan.Plan) (bool, error) {
	if stmt.Cmd != mysql.ComQuery {
		return false, rejectMulti("multi-shard command %d is not supported", stmt.Cmd)
	}
	if !supportedMerge(pl.Merge) {
		return false, rejectMulti("multi-shard merge is not implemented")
	}
	if len(pl.Targets) == 0 {
		return false, fmt.Errorf("execute: multi-shard plan has no targets")
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

	var fields []result.Field
	for _, target := range pl.Targets {
		stream, err := p.openDecoded(ctx, rt, stmt, target)
		if err != nil {
			var se *errs.MySQL
			if errors.As(err, &se) {
				// Nothing has been written yet; forward the backend ERR and keep
				// the session alive, matching the single-target raw path.
				cleanup(true)
				return false, mysql.WriteERR(rt.Front, se)
			}
			cleanup(true)
			return false, err
		}
		if fields == nil {
			fields = stream.Fields()
			if len(fields) == 0 {
				_ = stream.Close()
				cleanup(true)
				return false, fmt.Errorf("execute: shard %s returned no columns", target.Slice)
			}
		} else if !sameFields(fields, stream.Fields()) {
			shardFields := stream.Fields()
			_ = stream.Close()
			cleanup(true)
			return false, fmt.Errorf("execute: shard %s column metadata mismatch: %+v != %+v",
				target.Slice, shardFields, fields)
		}
		opened = append(opened, shardResult{slice: target.Slice, stream: stream})
	}

	// All headers are valid, so a partial result set is no longer possible.
	if err := writeResultHeader(rt.Front, fields); err != nil {
		cleanup(true)
		return false, err
	}
	streams := make([]result.Stream, 0, len(opened))
	for _, o := range opened {
		streams = append(streams, o.stream)
	}
	merged := merge.Concat(streams)
	if pl.Merge.Limit != nil {
		merged = merge.Limit(merged, pl.Merge.Limit.Offset, pl.Merge.Limit.Count)
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
		cells, err := encodeCells(row, len(fields))
		if err != nil {
			cleanup(true)
			return false, err
		}
		if err := writeFrontPayload(rt.Front, mysql.EncodeTextRow(cells)); err != nil {
			cleanup(true)
			return false, err
		}
	}
	if err := writeFrontPayload(rt.Front, mysql.EncodeEOF(frontendDeprecateEOF, mysql.ServerStatusAutocommit, 0)); err != nil {
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
	if rt.leases == nil {
		rt.leases = make(map[string]*lease)
	}
	l := rt.leases[target.Slice]
	acquired := false
	if l == nil {
		slice, err := p.cluster.Get(target.Slice)
		if err != nil {
			return nil, err
		}
		conn, pool, err := slice.Acquire(ctx, target.Role)
		if err != nil {
			return nil, err
		}
		l = &lease{slice: slice, conn: conn, pool: pool}
		rt.leases[target.Slice] = l
		acquired = true
	}

	db := target.DB
	if db == "" {
		db = rt.State.Database()
	}
	if err := l.conn.Align(ctx, backend.AlignSettings{Database: db, Charset: rt.State.Charset()}); err != nil {
		if acquired {
			_ = p.releaseOne(rt, target.Slice, false)
		}
		return nil, err
	}
	payload, err := p.payloadFor(rt, stmt, target)
	if err != nil {
		return nil, err
	}
	stream, err := l.conn.Exec(ctx, backend.ExecRequest{Payload: payload})
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

// supportedMerge reports merge features the current concat path can honor.
// LIMIT is applied after concat; ORDER BY/aggregate/group/distinct need the
// ordered/aggregate operators and are rejected by the router today.
func supportedMerge(m plan.MergeSpec) bool {
	return len(m.OrderBy) == 0 && len(m.Aggregates) == 0 &&
		len(m.GroupBy) == 0 && !m.Distinct && len(m.Output) == 0
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

func writeResultHeader(front *packet.Conn, fields []result.Field) error {
	if err := writeFrontPayload(front, mysql.EncodeColumnCount(uint64(len(fields)))); err != nil {
		return err
	}
	for _, f := range fields {
		def := mysql.EncodeColumnDefinition(mysql.Column{
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
		if err := writeFrontPayload(front, def); err != nil {
			return err
		}
	}
	return nil
}

func rejectMulti(format string, args ...any) error {
	return errs.Unsupported(format, args...)
}
