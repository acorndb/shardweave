package execute

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/result"
	"github.com/acorndb/shardweave/pkg/route"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
	"github.com/stretchr/testify/require"
)

type multiRouter struct {
	targets []route.Target
}

func (m multiRouter) Route(context.Context, *session.State, *sql.Statement) ([]route.Target, error) {
	return m.targets, nil
}

func multiTargets() []route.Target {
	return []route.Target{
		{Slice: "s0", Role: constant.RoleMaster},
		{Slice: "s1", Role: constant.RoleMaster},
	}
}

func newMultiPipeline(t *testing.T, tr0, tr1 *tracker, cfg0, cfg1 func(*fakeConn)) *Pipeline {
	t.Helper()
	g0 := sliceWith("s0", newPool(t, "s0-master", tr0, cfg0))
	g1 := sliceWith("s1", newPool(t, "s1-master", tr1, cfg1))
	p, err := New(clusterOf(t, g0, g1), plan.Builder{
		Router:   multiRouter{targets: multiTargets()},
		Sharding: true,
	})
	require.NoError(t, err)
	return p
}

// captureFront wires a frontend to an in-memory reader and returns the packets
// the executor writes. collect must be called after Handle returns.
func captureFront(t *testing.T) (*packet.Conn, func() [][]byte) {
	t.Helper()
	srv, cli := net.Pipe()
	front := packet.NewConn(srv)
	reader := packet.NewConn(cli)

	var mu sync.Mutex
	var pkts [][]byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			p, err := reader.ReadPacket()
			if err != nil {
				return
			}
			mu.Lock()
			pkts = append(pkts, append([]byte(nil), p...))
			mu.Unlock()
		}
	}()
	collect := func() [][]byte {
		_ = srv.Close()
		<-done
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), pkts...)
	}
	t.Cleanup(func() {
		_ = srv.Close()
		_ = cli.Close()
		<-done
	})
	return front, collect
}

func orderFields(id string) []result.Field {
	return []result.Field{
		{Name: id, OrgName: id, Table: "orders", OrgTable: "orders", DB: "bench", Type: 0x03, Charset: 63, ColumnLength: 11},
		{Name: "amount", OrgName: "amount", Table: "orders", OrgTable: "orders", DB: "bench", Type: 0x03, Charset: 63, ColumnLength: 11},
	}
}

func TestDispatch_Concat(t *testing.T) {
	tr0, tr1 := &tracker{}, &tracker{}
	p := newMultiPipeline(t, tr0, tr1,
		func(c *fakeConn) {
			c.fields = orderFields("id")
			c.rows = []result.Row{{[]byte("1"), []byte("10")}}
		},
		func(c *fakeConn) {
			c.fields = orderFields("id")
			c.rows = []result.Row{
				{[]byte("2"), []byte("20")},
				{[]byte("3"), []byte("30")},
			}
		})

	front, collect := captureFront(t)
	rt := newRuntime()
	rt.Front = front
	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT id, amount FROM orders")))

	pkts := collect()
	require.Len(t, pkts, 1+2+3+1, "column count + 2 columns + 3 rows + terminator")
	require.Equal(t, mysql.EncodeColumnCount(2), pkts[0])
	for i, want := range []string{"id", "amount"} {
		col, err := mysql.DecodeColumn(pkts[1+i])
		require.NoError(t, err)
		require.Equal(t, want, col.Name)
		require.Equal(t, uint16(63), col.Charset)
	}
	var rows [][]string
	for _, pkt := range pkts[3:6] {
		cells, err := mysql.DecodeTextRow(pkt, 2)
		require.NoError(t, err)
		rows = append(rows, []string{string(cells[0]), string(cells[1])})
	}
	require.Equal(t, [][]string{{"1", "10"}, {"2", "20"}, {"3", "30"}}, rows)
	require.Equal(t, mysql.EncodeEOF(true, mysql.ServerStatusAutocommit, 0), pkts[6])

	require.Equal(t, 1, tr0.last(t).execCalls)
	require.Equal(t, 1, tr1.last(t).execCalls)
	require.Equal(t, 1, tr0.last(t).resets, "successful concat releases every lease")
	require.Equal(t, 1, tr1.last(t).resets)
	require.Empty(t, rt.leases)
	require.Equal(t, int64(-1), rt.State.RowCount())
}

func TestDispatch_MetadataMismatch(t *testing.T) {
	tr0, tr1 := &tracker{}, &tracker{}
	p := newMultiPipeline(t, tr0, tr1,
		func(c *fakeConn) {
			c.fields = orderFields("id")
			c.rows = []result.Row{{[]byte("1"), []byte("10")}}
		},
		func(c *fakeConn) {
			c.fields = orderFields("order_id")
			c.rows = []result.Row{{[]byte("2"), []byte("20")}}
		})

	front, collect := captureFront(t)
	rt := newRuntime()
	rt.Front = front
	err := p.Handle(context.Background(), rt, query("SELECT id, amount FROM orders"))
	require.ErrorContains(t, err, "column metadata")

	require.Empty(t, collect(), "metadata must be checked before writing any packet")
	require.Empty(t, rt.leases)
	require.True(t, tr0.last(t).closed, "unread shard responses are discarded")
	require.True(t, tr1.last(t).closed)
	require.Equal(t, int64(-1), rt.State.RowCount())
}

func TestDispatch_ServerError(t *testing.T) {
	tr0, tr1 := &tracker{}, &tracker{}
	p := newMultiPipeline(t, tr0, tr1,
		func(c *fakeConn) {
			c.fields = orderFields("id")
		},
		func(c *fakeConn) {
			c.decodeErr = &errs.MySQL{Number: 1146, SQLState: "42S02", Message: "Table 'bench.orders' doesn't exist"}
		})

	front, collect := captureFront(t)
	rt := newRuntime()
	rt.Front = front
	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT id, amount FROM orders")))

	pkts := collect()
	require.Len(t, pkts, 1)
	require.Equal(t, byte(0xff), pkts[0][0], "backend error is forwarded as ERR, not a partial result set")
	var se *errs.MySQL
	require.ErrorAs(t, mysql.DecodeServerError(pkts[0]), &se)
	require.Equal(t, uint16(1146), se.Number)
	require.Contains(t, se.Message, "doesn't exist")
	require.Empty(t, rt.leases)
	require.Equal(t, int64(-1), rt.State.RowCount())
}

func TestDispatch_Rejects(t *testing.T) {
	p := newMultiPipeline(t, &tracker{}, &tracker{}, nil, nil)
	ctx := context.Background()

	t.Run("non_query", func(t *testing.T) {
		_, err := p.executePlan(ctx, newRuntime(), &sql.Statement{Cmd: mysql.ComStmtExecute}, &plan.Plan{
			Kind: plan.MultiShard, Targets: multiTargets(),
		})
		var rej *errs.MySQL
		require.ErrorAs(t, err, &rej)
		require.Equal(t, uint16(1235), rej.Number)
	})

	t.Run("merge_spec", func(t *testing.T) {
		_, err := p.executePlan(ctx, newRuntime(), query("SELECT * FROM orders"), &plan.Plan{
			Kind:    plan.MultiShard,
			Targets: multiTargets(),
			Merge:   plan.MergeSpec{GroupBy: []int{0}},
		})
		var rej *errs.MySQL
		require.ErrorAs(t, err, &rej)
		require.Contains(t, rej.Message, "merge")
	})

	t.Run("no_targets", func(t *testing.T) {
		_, err := p.executePlan(ctx, newRuntime(), query("SELECT * FROM orders"), &plan.Plan{Kind: plan.MultiShard})
		require.ErrorContains(t, err, "no targets")
	})
}

func TestDispatch_Ordered(t *testing.T) {
	tr0, tr1 := &tracker{}, &tracker{}
	p := newMultiPipeline(t, tr0, tr1,
		func(c *fakeConn) {
			c.fields = orderFields("id")
			c.rows = []result.Row{{[]byte("2"), []byte("20")}, {[]byte("4"), []byte("40")}}
		},
		func(c *fakeConn) {
			c.fields = orderFields("id")
			c.rows = []result.Row{{[]byte("1"), []byte("10")}, {[]byte("3"), []byte("30")}}
		})

	front, collect := captureFront(t)
	rt := newRuntime()
	rt.Front = front
	_, err := p.executePlan(context.Background(), rt, query("SELECT id, amount FROM orders ORDER BY id"),
		&plan.Plan{
			Kind:    plan.MultiShard,
			Targets: multiTargets(),
			Merge:   plan.MergeSpec{OrderBy: []plan.OrderKey{{Kind: plan.OrderByName, Name: "id"}}},
		})
	require.NoError(t, err)

	pkts := collect()
	require.Len(t, pkts, 1+2+4+1)
	var ids []string
	for _, pkt := range pkts[3:7] {
		cells, err := mysql.DecodeTextRow(pkt, 2)
		require.NoError(t, err)
		ids = append(ids, string(cells[0]))
	}
	require.Equal(t, []string{"1", "2", "3", "4"}, ids)
	require.Empty(t, rt.leases)
	require.Equal(t, 1, tr0.last(t).resets, "fully consumed ordered streams are released")
	require.Equal(t, 1, tr1.last(t).resets)
}

func TestDispatch_OrderedLimit(t *testing.T) {
	tr0, tr1 := &tracker{}, &tracker{}
	p := newMultiPipeline(t, tr0, tr1,
		func(c *fakeConn) {
			c.fields = orderFields("id")
			c.rows = []result.Row{{[]byte("2"), []byte("20")}, {[]byte("4"), []byte("40")}}
		},
		func(c *fakeConn) {
			c.fields = orderFields("id")
			c.rows = []result.Row{{[]byte("1"), []byte("10")}, {[]byte("3"), []byte("30")}}
		})

	front, collect := captureFront(t)
	rt := newRuntime()
	rt.Front = front
	_, err := p.executePlan(context.Background(), rt, query("SELECT id, amount FROM orders ORDER BY id LIMIT 2"),
		&plan.Plan{
			Kind:    plan.MultiShard,
			Targets: multiTargets(),
			Merge: plan.MergeSpec{
				OrderBy: []plan.OrderKey{{Kind: plan.OrderByName, Name: "id"}},
				Limit:   &plan.Limit{Count: 2},
			},
		})
	require.NoError(t, err)

	pkts := collect()
	require.Len(t, pkts, 1+2+2+1)
	var ids []string
	for _, pkt := range pkts[3:5] {
		cells, err := mysql.DecodeTextRow(pkt, 2)
		require.NoError(t, err)
		ids = append(ids, string(cells[0]))
	}
	require.Equal(t, []string{"1", "2"}, ids)
	require.True(t, tr0.last(t).closed, "limit may leave unread rows; discard")
	require.True(t, tr1.last(t).closed)
	require.Empty(t, rt.leases)
}

func TestDispatch_OrderedRejectsUnsupportedTypeBeforeHeader(t *testing.T) {
	tr0, tr1 := &tracker{}, &tracker{}
	strFields := []result.Field{{Name: "id", OrgName: "id", Type: 0xfd}}
	p := newMultiPipeline(t, tr0, tr1,
		func(c *fakeConn) {
			c.fields = strFields
			c.rows = []result.Row{{[]byte("a")}}
		},
		func(c *fakeConn) {
			c.fields = strFields
			c.rows = []result.Row{{[]byte("b")}}
		})

	front, collect := captureFront(t)
	rt := newRuntime()
	rt.Front = front
	_, err := p.executePlan(context.Background(), rt, query("SELECT id FROM orders ORDER BY id"),
		&plan.Plan{
			Kind:    plan.MultiShard,
			Targets: multiTargets(),
			Merge:   plan.MergeSpec{OrderBy: []plan.OrderKey{{Kind: plan.OrderByName, Name: "id"}}},
		})
	var rej *errs.MySQL
	require.ErrorAs(t, err, &rej)
	require.Equal(t, uint16(1235), rej.Number)
	require.Empty(t, collect(), "ORDER BY validation happens before any result packet")
	require.True(t, tr0.last(t).closed)
	require.True(t, tr1.last(t).closed)
	require.Empty(t, rt.leases)
}

func TestDispatch_Aggregate(t *testing.T) {
	tr0, tr1 := &tracker{}, &tracker{}
	fields := []result.Field{
		{Name: "COUNT(*)", OrgName: "COUNT(*)", Type: 0x08, Charset: 63},
		{Name: "SUM(amount)", OrgName: "SUM(amount)", Type: 0xf6, Charset: 63, Decimals: 0},
	}
	p := newMultiPipeline(t, tr0, tr1,
		func(c *fakeConn) {
			c.fields = fields
			c.rows = []result.Row{{[]byte("2"), []byte("50")}}
		},
		func(c *fakeConn) {
			c.fields = fields
			c.rows = []result.Row{{[]byte("3"), []byte("200")}}
		})

	front, collect := captureFront(t)
	rt := newRuntime()
	rt.Front = front
	_, err := p.executePlan(context.Background(), rt, query("SELECT COUNT(*), SUM(amount) FROM orders"),
		&plan.Plan{
			Kind:    plan.MultiShard,
			Targets: multiTargets(),
			Merge: plan.MergeSpec{Aggregates: []plan.AggregateSpec{
				{Func: plan.AggCount, Column: 0, Output: 0},
				{Func: plan.AggSum, Column: 1, Output: 1},
			}},
		})
	require.NoError(t, err)

	pkts := collect()
	require.Len(t, pkts, 1+2+1+1, "column count + 2 columns + 1 aggregate row + terminator")
	cells, err := mysql.DecodeTextRow(pkts[3], 2)
	require.NoError(t, err)
	require.Equal(t, "5", string(cells[0]))
	require.Equal(t, "250", string(cells[1]))
	require.Empty(t, rt.leases)
	require.Equal(t, 1, tr0.last(t).resets, "aggregate consumes every shard row")
	require.Equal(t, 1, tr1.last(t).resets)
}

func TestDispatch_Avg(t *testing.T) {
	tr0, tr1 := &tracker{}, &tracker{}
	fields := []result.Field{
		{Name: "AVG(amount)", OrgName: "AVG(amount)", Type: result.TypeNewDecimal, Charset: 63, Decimals: 4},
		{Name: "__sw_avg_sum_0", OrgName: "SUM(amount)", Type: result.TypeNewDecimal, Charset: 63},
		{Name: "__sw_avg_count_0", OrgName: "COUNT(amount)", Type: result.TypeLongLong, Charset: 63},
	}
	p := newMultiPipeline(t, tr0, tr1,
		func(c *fakeConn) {
			c.fields = fields
			c.rows = []result.Row{{[]byte("2.5000"), []byte("5"), []byte("2")}}
		},
		func(c *fakeConn) {
			c.fields = fields
			c.rows = []result.Row{{[]byte("4.0000"), []byte("8"), []byte("2")}}
		})

	front, collect := captureFront(t)
	rt := newRuntime()
	rt.Front = front
	_, err := p.executePlan(context.Background(), rt, query("SELECT AVG(amount) FROM orders"),
		&plan.Plan{
			Kind:    plan.MultiShard,
			Targets: multiTargets(),
			Merge: plan.MergeSpec{Aggregates: []plan.AggregateSpec{{
				Func: plan.AggAvg, Column: 1, CountColumn: 2, Output: 0,
			}}},
		})
	require.NoError(t, err)

	pkts := collect()
	require.Len(t, pkts, 1+1+1+1, "header + 1 output column + 1 row + terminator")
	cells, err := mysql.DecodeTextRow(pkts[2], 1)
	require.NoError(t, err)
	require.Equal(t, "3.2500", string(cells[0]))
	require.Empty(t, rt.leases)
	require.Equal(t, 1, tr0.last(t).resets)
	require.Equal(t, 1, tr1.last(t).resets)
}

func TestDispatch_AggregateLimitZero(t *testing.T) {
	tr0, tr1 := &tracker{}, &tracker{}
	fields := []result.Field{{Name: "COUNT(*)", OrgName: "COUNT(*)", Type: 0x08, Charset: 63}}
	p := newMultiPipeline(t, tr0, tr1,
		func(c *fakeConn) { c.fields = fields; c.rows = []result.Row{{[]byte("2")}} },
		func(c *fakeConn) { c.fields = fields; c.rows = []result.Row{{[]byte("3")}} })

	front, collect := captureFront(t)
	rt := newRuntime()
	rt.Front = front
	_, err := p.executePlan(context.Background(), rt, query("SELECT COUNT(*) FROM orders LIMIT 0"),
		&plan.Plan{
			Kind:    plan.MultiShard,
			Targets: multiTargets(),
			Merge: plan.MergeSpec{
				Aggregates: []plan.AggregateSpec{{Func: plan.AggCount, Column: 0, Output: 0}},
				Limit:      &plan.Limit{Count: 0},
			},
		})
	require.NoError(t, err)
	pkts := collect()
	require.Len(t, pkts, 1+1+1, "header + terminator only")
	require.True(t, tr0.last(t).closed)
	require.True(t, tr1.last(t).closed)
	require.Empty(t, rt.leases)
}

func TestDispatch_Limit(t *testing.T) {
	tr0, tr1 := &tracker{}, &tracker{}
	p := newMultiPipeline(t, tr0, tr1,
		func(c *fakeConn) {
			c.fields = orderFields("id")
			c.rows = []result.Row{{[]byte("1"), []byte("10")}, {[]byte("2"), []byte("20")}}
		},
		func(c *fakeConn) {
			c.fields = orderFields("id")
			c.rows = []result.Row{{[]byte("3"), []byte("30")}, {[]byte("4"), []byte("40")}}
		})

	front, collect := captureFront(t)
	rt := newRuntime()
	rt.Front = front
	_, err := p.executePlan(context.Background(), rt, query("SELECT id FROM orders LIMIT 3"), &plan.Plan{
		Kind:    plan.MultiShard,
		Targets: multiTargets(),
		Merge:   plan.MergeSpec{Limit: &plan.Limit{Count: 3}},
	})
	require.NoError(t, err)

	pkts := collect()
	require.Len(t, pkts, 1+2+3+1, "column count + 2 columns + 3 rows + terminator")
	var got []string
	for _, pkt := range pkts[3:6] {
		cells, err := mysql.DecodeTextRow(pkt, 2)
		require.NoError(t, err)
		got = append(got, string(cells[0]))
	}
	require.Equal(t, []string{"1", "2", "3"}, got)
	require.Empty(t, rt.leases)
	require.True(t, tr0.last(t).closed, "limit may leave unread rows; discard the lease")
	require.True(t, tr1.last(t).closed)
	require.Equal(t, int64(-1), rt.State.RowCount())
}

func TestDispatch_LimitZero(t *testing.T) {
	tr0, tr1 := &tracker{}, &tracker{}
	p := newMultiPipeline(t, tr0, tr1,
		func(c *fakeConn) {
			c.fields = orderFields("id")
			c.rows = []result.Row{{[]byte("1"), []byte("10")}}
		},
		func(c *fakeConn) {
			c.fields = orderFields("id")
			c.rows = []result.Row{{[]byte("2"), []byte("20")}}
		})

	front, collect := captureFront(t)
	rt := newRuntime()
	rt.Front = front
	_, err := p.executePlan(context.Background(), rt, query("SELECT id FROM orders LIMIT 0"), &plan.Plan{
		Kind:    plan.MultiShard,
		Targets: multiTargets(),
		Merge:   plan.MergeSpec{Limit: &plan.Limit{Count: 0}},
	})
	require.NoError(t, err)
	pkts := collect()
	require.Len(t, pkts, 1+2+1, "header + terminator only")
	require.Empty(t, rt.leases)
	require.True(t, tr0.last(t).closed)
	require.True(t, tr1.last(t).closed)
}

func TestDispatch_NoColumns(t *testing.T) {
	tr0, tr1 := &tracker{}, &tracker{}
	p := newMultiPipeline(t, tr0, tr1,
		func(c *fakeConn) { c.rows = []result.Row{{[]byte("1")}} },
		func(c *fakeConn) { c.fields = orderFields("id") })

	rt := newRuntime()
	rt.Front, _ = captureFront(t)
	err := p.Handle(context.Background(), rt, query("SELECT id FROM orders"))
	require.ErrorContains(t, err, "no columns")
	require.Empty(t, rt.leases)
	require.True(t, tr0.last(t).closed, "the column-less shard response is discarded")
}

func TestEncodeCells(t *testing.T) {
	cells, err := encodeCells(result.Row{nil, []byte("a"), "b"}, 3)
	require.NoError(t, err)
	require.Nil(t, cells[0])
	require.Equal(t, []byte("a"), cells[1])
	require.Equal(t, []byte("b"), cells[2])

	_, err = encodeCells(result.Row{[]byte("a")}, 2)
	require.ErrorContains(t, err, "want 2")

	_, err = encodeCells(result.Row{42}, 1)
	require.ErrorContains(t, err, "unsupported result value type")
}

func TestDispatch_RowError(t *testing.T) {
	tr0, tr1 := &tracker{}, &tracker{}
	p := newMultiPipeline(t, tr0, tr1,
		func(c *fakeConn) {
			c.fields = orderFields("id")
			c.rows = []result.Row{{[]byte("1"), []byte("10")}}
		},
		func(c *fakeConn) {
			c.fields = orderFields("id")
			c.rowErr = errors.New("connection reset")
		})

	front, collect := captureFront(t)
	rt := newRuntime()
	rt.Front = front
	err := p.Handle(context.Background(), rt, query("SELECT id, amount FROM orders"))
	require.ErrorContains(t, err, "connection reset")
	require.NotEmpty(t, collect())
	require.Empty(t, rt.leases)
	require.True(t, tr0.last(t).closed, "partial result must not return the backend to the pool")
	require.True(t, tr1.last(t).closed)
	require.Equal(t, int64(-1), rt.State.RowCount())
}
