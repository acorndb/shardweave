package execute

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/acorndb/shardweave/pkg/backend"
	"github.com/acorndb/shardweave/pkg/constant"
	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/result"
	"github.com/acorndb/shardweave/pkg/route"
	"github.com/acorndb/shardweave/pkg/route/source"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
	"github.com/stretchr/testify/require"
)

type fakeConn struct {
	tag string

	mu           sync.Mutex
	status       uint16
	serverErr    bool
	affectedRows uint64
	lastInsertID uint64
	resultSet    bool
	prepareID    uint32
	alignErr     error
	execErr      error
	execCalls    int
	aligns       int
	resets       int
	closed       bool
	lastReq      backend.ExecRequest
	lastAlign    backend.AlignSettings
	execHook     func()

	fields     []result.Field
	rows       []result.Row
	decodeErr  error
	rowErr     error
	undrained  bool
	forwardErr error
}

func (c *fakeConn) Ping(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return nil
}

func (c *fakeConn) Exec(_ context.Context, req backend.ExecRequest) (result.Stream, error) {
	c.mu.Lock()
	c.execCalls++
	c.lastReq = req
	status, serverErr, err := c.status, c.serverErr, c.execErr
	affected, lastID, resultSet := c.affectedRows, c.lastInsertID, c.resultSet
	prepareID := c.prepareID
	fields, rows, decodeErr, rowErr := c.fields, c.rows, c.decodeErr, c.rowErr
	undrained, forwardErr := c.undrained, c.forwardErr
	hook := c.execHook
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return nil, err
	}
	return &fakeStream{
		status:       status,
		serverErr:    serverErr,
		affectedRows: affected,
		lastInsertID: lastID,
		resultSet:    resultSet,
		prepareID:    prepareID,
		fields:       fields,
		rows:         rows,
		decodeErr:    decodeErr,
		rowErr:       rowErr,
		undrained:    undrained,
		forwardErr:   forwardErr,
	}, nil
}

func (c *fakeConn) Align(_ context.Context, s backend.AlignSettings) error {
	c.mu.Lock()
	c.aligns++
	c.lastAlign = s
	err := c.alignErr
	c.mu.Unlock()
	return err
}

func (c *fakeConn) Reset(context.Context) error {
	c.mu.Lock()
	c.resets++
	c.mu.Unlock()
	return nil
}

func (c *fakeConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

type fakeStream struct {
	status       uint16
	serverErr    bool
	affectedRows uint64
	lastInsertID uint64
	resultSet    bool
	prepareID    uint32
	closed       bool

	fields     []result.Field
	rows       []result.Row
	decodeErr  error
	rowErr     error
	undrained  bool
	forwardErr error
}

var (
	_ prepareForwarder = (*fakeStream)(nil)
	_ decodedStreamer  = (*fakeStream)(nil)
)

func (s *fakeStream) ForwardPrepareTo(*packet.Conn, uint32) (mysql.ResponseInfo, uint32, error) {
	return mysql.ResponseInfo{ServerErr: s.serverErr}, s.prepareID, nil
}

func (s *fakeStream) ForwardTo(context.Context, *packet.Conn) (mysql.ResponseInfo, error) {
	if s.forwardErr != nil {
		return mysql.ResponseInfo{}, s.forwardErr
	}
	return mysql.ResponseInfo{
		Status:       s.status,
		ServerErr:    s.serverErr,
		AffectedRows: s.affectedRows,
		LastInsertID: s.lastInsertID,
		ResultSet:    s.resultSet,
	}, nil
}

func (s *fakeStream) Decode() (result.Stream, error) {
	if s.decodeErr != nil {
		return nil, s.decodeErr
	}
	return &fakeResultStream{fields: s.fields, rows: s.rows, rowErr: s.rowErr}, nil
}

func (s *fakeStream) Drained() bool { return !s.undrained }

func (s *fakeStream) Fields() []result.Field { return s.fields }

func (s *fakeStream) Next() (result.Row, error) { return nil, io.EOF }

func (s *fakeStream) Close() error {
	s.closed = true
	return nil
}

type fakeResultStream struct {
	fields []result.Field
	rows   []result.Row
	rowErr error
	i      int
	closed bool
}

func (s *fakeResultStream) Fields() []result.Field { return s.fields }

func (s *fakeResultStream) Next() (result.Row, error) {
	if s.rowErr != nil && s.i == len(s.rows) {
		return nil, s.rowErr
	}
	if s.i >= len(s.rows) {
		return nil, io.EOF
	}
	row := s.rows[s.i]
	s.i++
	return row, nil
}

func (s *fakeResultStream) Close() error {
	s.closed = true
	return nil
}

type tracker struct {
	mu    sync.Mutex
	conns []*fakeConn
}

func (t *tracker) add(c *fakeConn) {
	t.mu.Lock()
	t.conns = append(t.conns, c)
	t.mu.Unlock()
}

func (t *tracker) opens() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.conns)
}

func (t *tracker) last(tb testing.TB) *fakeConn {
	tb.Helper()
	t.mu.Lock()
	defer t.mu.Unlock()
	require.NotEmpty(tb, t.conns)
	return t.conns[len(t.conns)-1]
}

func newPool(t *testing.T, tag string, tr *tracker, configure func(*fakeConn)) *backend.Pool {
	t.Helper()
	p := backend.NewPool(backend.PoolConfig{Capacity: 0, MaxCapacity: 4, AcquireTimeout: time.Second},
		func(context.Context) (backend.Conn, error) {
			c := &fakeConn{tag: tag, status: mysql.ServerStatusAutocommit}
			if configure != nil {
				configure(c)
			}
			tr.add(c)
			return c, nil
		})
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func sliceWith(name string, master *backend.Pool, slaves ...*backend.Pool) *backend.Slice {
	return &backend.Slice{Name: name, Master: master, Slaves: slaves}
}

func clusterOf(t *testing.T, slices ...*backend.Slice) *backend.Cluster {
	t.Helper()
	c, err := backend.NewCluster(slices...)
	require.NoError(t, err)
	return c
}

func newRuntime() *Runtime {
	return &Runtime{State: session.NewState()}
}

func query(raw string) *sql.Statement {
	return &sql.Statement{
		Cmd:     mysql.ComQuery,
		Payload: append([]byte{mysql.ComQuery}, []byte(raw)...),
		Raw:     raw,
		Kind:    sql.KindQuery,
	}
}

func prepareQuery(raw string) *sql.Statement {
	return &sql.Statement{
		Cmd:     mysql.ComStmtPrepare,
		Payload: append([]byte{mysql.ComStmtPrepare}, []byte(raw)...),
		Raw:     raw,
	}
}

func closeQuery() *sql.Statement {
	return &sql.Statement{
		Cmd:     mysql.ComStmtClose,
		Payload: []byte{mysql.ComStmtClose, 0x01, 0x00, 0x00, 0x00},
	}
}

func boolPtr(v bool) *bool { return &v }

func TestNew_Errors(t *testing.T) {
	_, err := New(nil, plan.Builder{Router: source.AllMaster{}})
	require.Error(t, err)

	tr := &tracker{}
	c := clusterOf(t, sliceWith("default", newPool(t, "master", tr, nil)))
	_, err = New(c, plan.Builder{})
	require.Error(t, err)
}

func TestPipeline_Close(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, nil))),
		plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)
	require.NoError(t, p.Close())
}

func TestPipeline_SingleUsesMaster(t *testing.T) {
	tr := &tracker{}
	g := sliceWith("default",
		newPool(t, "master", tr, nil),
		newPool(t, "slave", tr, nil))
	p, err := New(clusterOf(t, g), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	c := tr.last(t)
	require.Equal(t, "master", c.tag)
	require.Equal(t, 1, c.execCalls)
	require.False(t, c.closed, "clean connection must return to the pool")
	require.Empty(t, rt.leases)
}

func TestPipeline_RWSplitRoutes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "select_slave", raw: "SELECT 1", want: "slave"},
		{name: "insert_master", raw: "INSERT INTO t VALUES (1)", want: "master"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &tracker{}
			g := sliceWith("default",
				newPool(t, "master", tr, nil),
				newPool(t, "slave", tr, nil))
			p, err := New(clusterOf(t, g), plan.Builder{Router: source.Pick{}})
			require.NoError(t, err)

			require.NoError(t, p.Handle(context.Background(), newRuntime(), query(tc.raw)))
			require.Equal(t, tc.want, tr.last(t).tag)
		})
	}
}

func TestPipeline_StickyKeepsLease(t *testing.T) {
	tr := &tracker{}
	g := sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
		c.status = mysql.ServerStatusAutocommit | mysql.ServerStatusInTrans
	}))
	p, err := New(clusterOf(t, g), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	require.NoError(t, p.Handle(context.Background(), rt, query("BEGIN")))
	require.Len(t, rt.leases, 1)
	require.NotNil(t, rt.leases["default"])
	require.True(t, rt.State.InTx())
	c := tr.last(t)
	require.Equal(t, 1, c.execCalls)
	require.False(t, c.closed)

	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	require.Equal(t, 2, c.execCalls)
	require.Equal(t, 1, tr.opens(), "sticky must reuse the lease")

	require.NoError(t, p.Release(rt, false))
	require.Empty(t, rt.leases)
	require.True(t, c.closed, "releasing mid-transaction must discard the connection")
}

func TestPipeline_ReleaseDiscard(t *testing.T) {
	tr := &tracker{}
	g := sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
		c.status = mysql.ServerStatusAutocommit | mysql.ServerStatusInTrans
	}))
	p, err := New(clusterOf(t, g), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	require.NoError(t, p.Handle(context.Background(), rt, query("BEGIN")))
	require.NoError(t, p.Release(rt, true))
	c := tr.last(t)
	require.True(t, c.closed)
	require.Equal(t, 0, c.resets)
	require.Empty(t, rt.leases)
}

func TestPipeline_AlignFailureReleases(t *testing.T) {
	tr := &tracker{}
	g := sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
		c.alignErr = errors.New("align failed")
	}))
	p, err := New(clusterOf(t, g), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	require.Error(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	require.Empty(t, rt.leases)
	c := tr.last(t)
	require.True(t, c.closed, "an align failure means the connection state is unknown")
	require.Equal(t, 0, c.execCalls)
}

func TestPipeline_ExecFailureReleases(t *testing.T) {
	tr := &tracker{}
	g := sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
		c.execErr = errors.New("exec failed")
	}))
	p, err := New(clusterOf(t, g), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	require.Error(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	require.Empty(t, rt.leases)
	c := tr.last(t)
	require.Equal(t, 1, c.execCalls)
	require.True(t, c.closed, "an exec failure discards the connection")
}

func TestPipeline_EffectAppliedAfterSuccess(t *testing.T) {
	tr := &tracker{}
	g := sliceWith("default", newPool(t, "master", tr, nil))
	p, err := New(clusterOf(t, g), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	stmt := query("SET autocommit=0")
	stmt.Effect = sql.Effect{UseDB: "app", Charset: "utf8mb4", Autocommit: boolPtr(false)}
	require.NoError(t, p.Handle(context.Background(), rt, stmt))

	require.Equal(t, "app", rt.State.Database())
	require.Equal(t, "utf8mb4", rt.State.Charset())
	require.False(t, rt.State.Autocommit())
	require.Len(t, rt.leases, 1, "autocommit=0 must keep the lease")
	require.NoError(t, p.Release(rt, false))
}

func TestPipeline_EffectSkippedOnServerErr(t *testing.T) {
	tr := &tracker{}
	g := sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
		c.serverErr = true
	}))
	p, err := New(clusterOf(t, g), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	stmt := query("SET autocommit=0")
	stmt.Effect = sql.Effect{UseDB: "app", Autocommit: boolPtr(false)}
	require.NoError(t, p.Handle(context.Background(), rt, stmt))

	require.Empty(t, rt.State.Database())
	require.True(t, rt.State.Autocommit())
	require.Empty(t, rt.leases, "non-sticky server error must release")
	require.False(t, tr.last(t).closed, "a backend ERR leaves the connection reusable")
}

func TestPipeline_RewriteConnectionID(t *testing.T) {
	st := session.NewState()
	st.SetConnectionID(7)
	stmt := query("SELECT CONNECTION_ID()")
	rewriteSessionFunc(st, stmt)

	want := "SELECT CAST(7 AS UNSIGNED) AS `CONNECTION_ID()`"
	require.Equal(t, want, string(stmt.Payload[1:]))
	require.Equal(t, want, stmt.Raw)
	require.Nil(t, stmt.EnsureClass().SessionFunc, "the rewritten statement no longer references the session function")
}

func TestPipeline_KillRejected(t *testing.T) {
	tr := &tracker{}
	g := sliceWith("default", newPool(t, "master", tr, nil))
	p, err := New(clusterOf(t, g), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	err = p.Handle(context.Background(), newRuntime(), query("KILL 1"))
	var rej *errs.MySQL
	require.ErrorAs(t, err, &rej)
	require.Equal(t, uint16(1235), rej.Number)
	require.Contains(t, rej.Message, "KILL")
	require.Zero(t, tr.opens(), "KILL must never reach a pooled backend")
}

func TestPipeline_ExplainRouteStatusInTransaction(t *testing.T) {
	tr := &tracker{}
	g := sliceWith("default", newPool(t, "master", tr, nil))
	p, err := New(clusterOf(t, g), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	front, collect := captureFront(t)
	rt := newRuntime()
	rt.Front = front
	rt.State.SetInTx(true)

	stmt := query("SELECT 1")
	stmt.Kind = sql.KindExplainRoute
	require.NoError(t, p.Handle(context.Background(), rt, stmt))

	pkts := collect()
	require.NotEmpty(t, pkts)
	require.Equal(t,
		mysql.EncodeEOF(true, mysql.ServerStatusAutocommit|mysql.ServerStatusInTrans, 0),
		pkts[len(pkts)-1],
		"a synthetic result set inside a transaction must report SERVER_STATUS_IN_TRANS")
	require.Equal(t, int64(-1), rt.State.RowCount())
}

func TestPipeline_SetNamesStatusInTransaction(t *testing.T) {
	tr := &tracker{}
	g := sliceWith("default", newPool(t, "master", tr, nil))
	p, err := New(clusterOf(t, g), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	front, collect := captureFront(t)
	rt := newRuntime()
	rt.Front = front
	rt.State.SetInTx(true)

	require.NoError(t, p.Handle(context.Background(), rt, query("SET NAMES utf8mb4")))
	pkts := collect()
	require.Len(t, pkts, 1)
	require.Equal(t, mysql.EncodeOK(0, 0, mysql.ServerStatusAutocommit|mysql.ServerStatusInTrans, 0), pkts[0])
	require.Zero(t, tr.opens(), "SET NAMES is answered locally")
}

func TestPipeline_ServerErrKeepsInTx(t *testing.T) {
	tr := &tracker{}
	g := sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
		c.serverErr = true
	}))
	p, err := New(clusterOf(t, g), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	rt.State.SetInTx(true)
	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	require.True(t, rt.State.InTx(), "ERR must not clear transaction state")
	require.Len(t, rt.leases, 1)
	require.False(t, tr.last(t).closed)
	require.NoError(t, p.Release(rt, true))
}

func TestPipeline_ExecFlags(t *testing.T) {
	cases := []struct {
		name    string
		stmt    *sql.Statement
		prepare bool
		noReply bool
	}{
		{name: "prepare", stmt: prepareQuery("SELECT ?"), prepare: true},
		{name: "close", stmt: closeQuery(), noReply: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &tracker{}
			g := sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
				c.status = mysql.ServerStatusAutocommit | mysql.ServerStatusInTrans
			}))
			p, err := New(clusterOf(t, g), plan.Builder{Router: source.AllMaster{}})
			require.NoError(t, err)

			rt := newRuntime()
			require.NoError(t, p.Handle(context.Background(), rt, tc.stmt))
			c := tr.last(t)
			require.Equal(t, tc.prepare, c.lastReq.Prepare)
			require.Equal(t, tc.noReply, c.lastReq.NoReply)
			require.False(t, rt.State.InTx(), "prepare/close must not update transaction state")
		})
	}
}

type fakeRouter struct {
	targets []route.Target
	err     error
}

func (r fakeRouter) Route(context.Context, *session.State, *sql.Statement) ([]route.Target, error) {
	return r.targets, r.err
}

type seqRouter struct {
	mu      sync.Mutex
	targets [][]route.Target
	i       int
}

func (r *seqRouter) Route(context.Context, *session.State, *sql.Statement) ([]route.Target, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.targets) == 0 {
		return nil, errors.New("no targets")
	}
	idx := r.i
	if idx >= len(r.targets) {
		idx = len(r.targets) - 1
	} else {
		r.i++
	}
	return r.targets[idx], nil
}

func TestPipeline_RejectPlan(t *testing.T) {
	tr := &tracker{}
	g := sliceWith("default", newPool(t, "master", tr, nil))
	p, err := New(clusterOf(t, g), plan.Builder{Router: fakeRouter{targets: []route.Target{
		{Slice: "s0", Role: constant.RoleMaster},
		{Slice: "s1", Role: constant.RoleMaster},
	}}})
	require.NoError(t, err)

	err = p.Handle(context.Background(), newRuntime(), query("SELECT 1"))
	var rej *errs.MySQL
	require.ErrorAs(t, err, &rej)
	require.Equal(t, 0, tr.opens())
}

func TestPipeline_MultiSliceLeases(t *testing.T) {
	tr0, tr1 := &tracker{}, &tracker{}
	hold := func(c *fakeConn) {
		c.status = mysql.ServerStatusAutocommit | mysql.ServerStatusInTrans
	}
	s0 := sliceWith("s0", newPool(t, "s0-master", tr0, hold))
	s1 := sliceWith("s1", newPool(t, "s1-master", tr1, hold))
	router := &seqRouter{targets: [][]route.Target{
		{{Slice: "s0", Role: constant.RoleMaster}},
		{{Slice: "s1", Role: constant.RoleMaster}},
	}}
	p, err := New(clusterOf(t, s0, s1), plan.Builder{Router: router})
	require.NoError(t, err)

	rt := newRuntime()
	rt.State.SetInTx(true)
	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	require.Len(t, rt.leases, 2)
	require.Equal(t, 1, tr0.last(t).execCalls)
	require.Equal(t, 1, tr1.last(t).execCalls)

	require.NoError(t, p.Release(rt, false))
	require.Empty(t, rt.leases)
	require.True(t, tr0.last(t).closed, "mid-transaction release discards")
	require.True(t, tr1.last(t).closed, "mid-transaction release discards")
}

func TestPipeline_UnknownSlice(t *testing.T) {
	tr := &tracker{}
	g := sliceWith("default", newPool(t, "master", tr, nil))
	p, err := New(clusterOf(t, g), plan.Builder{Router: fakeRouter{targets: []route.Target{
		{Slice: "nope", Role: constant.RoleMaster},
	}}})
	require.NoError(t, err)

	err = p.Handle(context.Background(), newRuntime(), query("SELECT 1"))
	require.ErrorContains(t, err, "not found")
	require.Equal(t, 0, tr.opens())
}

func TestPipeline_NilInputs(t *testing.T) {
	tr := &tracker{}
	g := sliceWith("default", newPool(t, "master", tr, nil))
	p, err := New(clusterOf(t, g), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	require.Error(t, p.Handle(context.Background(), nil, query("SELECT 1")))
	require.Error(t, p.Handle(context.Background(), newRuntime(), nil))
	require.NoError(t, p.Release(nil, false))
	require.NoError(t, p.Release(newRuntime(), true))
}

func TestPipeline_CapturesOKMetadata(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
		c.affectedRows = 3
		c.lastInsertID = 42
	}))), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	require.NoError(t, p.Handle(context.Background(), rt, query("INSERT INTO t VALUES (1)")))
	require.Equal(t, uint64(42), rt.State.LastInsertID())
	require.Equal(t, int64(3), rt.State.RowCount())
}

func TestPipeline_LastInsertIDRewrite(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
		c.resultSet = true
	}))), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	rt.State.SetLastInsertID(42)
	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT LAST_INSERT_ID() AS id")))
	require.Equal(t, "SELECT CAST(42 AS UNSIGNED) AS `id`", string(tr.last(t).lastReq.Payload[1:]))
	require.Equal(t, int64(-1), rt.State.RowCount())
}

func TestPipeline_RowCountRewrite(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
		c.resultSet = true
	}))), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	rt.State.SetRowCount(3)
	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT ROW_COUNT()")))
	require.Equal(t, "SELECT CAST(3 AS SIGNED) AS `ROW_COUNT()`", string(tr.last(t).lastReq.Payload[1:]))
	require.Equal(t, int64(-1), rt.State.RowCount(), "the SELECT itself resets ROW_COUNT to -1")
}

func TestPipeline_ResultSetResetsRowCount(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
		c.resultSet = true
	}))), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	rt.State.SetRowCount(5)
	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	require.Equal(t, int64(-1), rt.State.RowCount())
}

func TestPipeline_ServerErrorResetsRowCount(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
		c.serverErr = true
	}))), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	rt.State.SetRowCount(5)
	rt.State.SetLastInsertID(7)
	require.NoError(t, p.Handle(context.Background(), rt, query("INSERT INTO t VALUES (1)")))
	require.Equal(t, int64(-1), rt.State.RowCount())
	require.Equal(t, uint64(7), rt.State.LastInsertID(), "failed statement must not change LAST_INSERT_ID")
}

func TestPipeline_DDLResetsRowCountToZero(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, nil))),
		plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	rt.State.SetRowCount(5)
	require.NoError(t, p.Handle(context.Background(), rt, query("CREATE TABLE t (id INT)")))
	require.Equal(t, int64(0), rt.State.RowCount())
}

func TestPipeline_PreparedExecuteUsesAffectedRows(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
		c.affectedRows = 2
	}))), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	stmt := &sql.Statement{Cmd: mysql.ComStmtExecute, Payload: []byte{mysql.ComStmtExecute}}
	require.NoError(t, p.Handle(context.Background(), rt, stmt))
	require.Equal(t, int64(2), rt.State.RowCount())
}

type sqlTargetRouter struct{ sql string }

func (r sqlTargetRouter) Route(context.Context, *session.State, *sql.Statement) ([]route.Target, error) {
	return []route.Target{{Slice: route.DefaultSlice, Role: constant.RoleMaster, SQL: r.sql}}, nil
}

func TestPayloadFor(t *testing.T) {
	original := append([]byte{mysql.ComQuery}, []byte("SELECT 1")...)
	stmt := &sql.Statement{Cmd: mysql.ComQuery, Payload: original, Raw: "SELECT 1"}
	rt := newRuntime()
	p := &Pipeline{}

	got, err := p.payloadFor(rt, stmt, route.Target{SQL: "SELECT 2"})
	require.NoError(t, err)
	require.Equal(t, append([]byte{mysql.ComQuery}, []byte("SELECT 2")...), got)

	got, err = p.payloadFor(rt, stmt, route.Target{})
	require.NoError(t, err)
	require.Equal(t, original, got, "empty target SQL must fall back to the original payload")

	execStmt := &sql.Statement{
		Cmd: mysql.ComStmtExecute, StmtID: 1,
		Payload: []byte{mysql.ComStmtExecute, 0x01, 0x00, 0x00, 0x00},
	}
	require.NoError(t, rt.State.Stmts().Add(&session.Stmt{
		ID: 1, Slice: "default", Targets: map[string]uint32{"default": 9},
	}))
	ps := &Pipeline{sharding: true}
	got, err = ps.payloadFor(rt, execStmt, route.Target{Slice: "default", SQL: "ignored"})
	require.NoError(t, err)
	require.Equal(t, uint32(9), binary.LittleEndian.Uint32(got[1:5]),
		"prepared commands must use the backend statement id, not Target.SQL")

	_, err = ps.payloadFor(rt, &sql.Statement{Cmd: mysql.ComStmtExecute, StmtID: 99, Payload: execStmt.Payload},
		route.Target{Slice: "default"})
	require.Error(t, err)
}

func TestPipeline_PrepareLifecycle(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
		c.prepareID = 9
		c.resultSet = true
	}))), plan.Builder{Router: source.AllMaster{}, Sharding: true})
	require.NoError(t, err)

	rt := newRuntime()
	prep := &sql.Statement{
		Cmd:     mysql.ComStmtPrepare,
		Raw:     "SELECT amount FROM orders WHERE user_id = ?",
		Payload: append([]byte{mysql.ComStmtPrepare}, []byte("SELECT amount FROM orders WHERE user_id = ?")...),
	}
	require.NoError(t, p.Handle(context.Background(), rt, prep))
	require.True(t, rt.State.NeedsSticky())
	require.False(t, rt.State.Reserved(), "the stmt pin must not look like a reserved SQL class")
	require.Len(t, rt.leases, 1, "prepare must pin the backend connection")

	entry, ok := rt.State.Stmts().Get(1)
	require.True(t, ok)
	require.Equal(t, "default", entry.Slice)
	require.Equal(t, uint32(9), entry.Targets["default"])

	exec := &sql.Statement{
		Cmd:     mysql.ComStmtExecute,
		StmtID:  1,
		Payload: []byte{mysql.ComStmtExecute, 0x01, 0x00, 0x00, 0x00},
	}
	require.NoError(t, p.Handle(context.Background(), rt, exec))
	require.Equal(t, uint32(9), binary.LittleEndian.Uint32(tr.last(t).lastReq.Payload[1:5]))

	closeStmt := &sql.Statement{
		Cmd:     mysql.ComStmtClose,
		StmtID:  1,
		Payload: []byte{mysql.ComStmtClose, 0x01, 0x00, 0x00, 0x00},
	}
	require.NoError(t, p.Handle(context.Background(), rt, closeStmt))
	require.Zero(t, rt.State.Stmts().Len())
	require.False(t, rt.State.NeedsSticky(), "closing the last stmt releases the pin")
	require.Empty(t, rt.leases, "closing the last stmt releases the backend lease")
	require.False(t, tr.last(t).closed, "a closed stmt leaves the connection clean")
}

func TestPipeline_UsesTargetSQL(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, nil))),
		plan.Builder{Router: sqlTargetRouter{sql: "SELECT 2"}})
	require.NoError(t, err)

	require.NoError(t, p.Handle(context.Background(), newRuntime(), query("SELECT 1")))
	got := tr.last(t).lastReq.Payload
	require.Equal(t, append([]byte{mysql.ComQuery}, []byte("SELECT 2")...), got)
}

func TestPipeline_MultiTargetRequiresFrontend(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, nil))),
		plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	_, err = p.executePlan(context.Background(), newRuntime(), query("SELECT 1"), &plan.Plan{
		Kind: plan.MultiShard,
		Targets: []route.Target{
			{Slice: "s0", Role: constant.RoleMaster},
			{Slice: "s1", Role: constant.RoleMaster},
		},
	})
	require.ErrorContains(t, err, "requires a frontend")
}

func TestExplainRows(t *testing.T) {
	stmt := query("SELECT 1")

	cols, rows := explainRows(stmt, &plan.Plan{
		Kind:    plan.SingleShard,
		Targets: []route.Target{{Slice: "s0", Role: constant.RoleSlave}},
	}, nil)
	require.Equal(t, []string{"kind", "slice", "role", "db", "sql", "reason"}, cols)
	require.Equal(t, [][]string{{"SingleShard", "s0", "slave", "", "SELECT 1", ""}}, rows)

	_, rows = explainRows(stmt, &plan.Plan{
		Kind:    plan.SingleShard,
		Targets: []route.Target{{Slice: "s1", Role: constant.RoleMaster, SQL: "SELECT 2"}},
	}, nil)
	require.Equal(t, "SELECT 2", rows[0][4])

	_, rows = explainRows(stmt, &plan.Plan{
		Kind: plan.Reject, Reject: &errs.MySQL{Message: "no key"},
	}, nil)
	require.Equal(t, "Reject", rows[0][0])
	require.Equal(t, "no key", rows[0][5])

	_, rows = explainRows(stmt, nil, &errs.MySQL{Message: "bad route"})
	require.Equal(t, "Reject", rows[0][0])
	require.Equal(t, "bad route", rows[0][5])

	_, rows = explainRows(stmt, nil, errors.New("boom"))
	require.Equal(t, "Error", rows[0][0])
	require.Contains(t, rows[0][5], "boom")
}

func TestPipeline_ExplainRouteWritesResult(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, nil))),
		plan.Builder{Router: multiRouter{targets: multiTargets()}, Sharding: true})
	require.NoError(t, err)

	front, collect := captureFront(t)
	rt := newRuntime()
	rt.Front = front
	stmt := query("SELECT * FROM orders")
	stmt.Kind = sql.KindExplainRoute
	require.NoError(t, p.Handle(context.Background(), rt, stmt))
	require.NotEmpty(t, collect())
	require.Equal(t, int64(-1), rt.State.RowCount())
}

func TestPipeline_DirtyStatementDiscards(t *testing.T) {
	cases := []string{
		// "SET SESSION sort_buffer_size = 1" moved to the rejection test:
		// non-whitelisted session variables are rejected before execution.
		"SELECT 1; SET SESSION sort_buffer_size = 1",
		"/*!40101 SET SESSION sort_buffer_size=1 */",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			tr := &tracker{}
			p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, nil))),
				plan.Builder{Router: source.AllMaster{}})
			require.NoError(t, err)

			rt := newRuntime()
			require.NoError(t, p.Handle(context.Background(), rt, query(raw)))
			require.Empty(t, rt.leases)
			require.True(t, tr.last(t).closed, "dirty session state must discard the connection")
		})
	}
}

func TestPipeline_SessionVarReplay(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, nil))),
		plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	setTZ := query("SET time_zone = '+08:00'")
	setTZ.Effect = setTZ.EnsureClass().Effect()
	require.NoError(t, p.Handle(context.Background(), rt, setTZ))
	require.Empty(t, rt.leases, "whitelisted vars must not pin the session")
	require.False(t, tr.last(t).closed, "whitelisted vars must keep the connection reusable")

	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	require.Equal(t, "+08:00", tr.last(t).lastAlign.Vars["time_zone"],
		"the next query must replay the session variable during Align")
	require.Equal(t, 1, tr.opens(), "the connection must be reused")
}

func TestPipeline_SetNamesAnsweredLocally(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, nil))),
		plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	frontRaw, frontPeer := net.Pipe()
	t.Cleanup(func() { _ = frontRaw.Close(); _ = frontPeer.Close() })
	rt := &Runtime{Front: packet.NewConn(frontRaw), State: session.NewState()}

	stmt := query("SET NAMES utf8mb4")
	stmt.Effect = stmt.EnsureClass().Effect()
	okCh := make(chan error, 1)
	go func() { okCh <- p.Handle(context.Background(), rt, stmt) }()
	peer := packet.NewConn(frontPeer)
	_, rerr := peer.ReadPacket()
	require.NoError(t, rerr)
	require.NoError(t, <-okCh)
	require.Equal(t, 0, tr.opens(), "SET NAMES must not reach a backend")
	require.Equal(t, "utf8mb4", rt.State.Charset())
}

func TestPipeline_NonWhitelistedSessionVarRejected(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, nil))),
		plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	err = p.Handle(context.Background(), rt, query("SET SESSION sort_buffer_size = 1"))
	var me *errs.MySQL
	require.ErrorAs(t, err, &me)
	require.Equal(t, uint16(1235), me.Number)
	require.Equal(t, 0, tr.opens(), "the statement must not reach a backend")
}

func TestPipeline_CleanStatementReturnsConnection(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, nil))),
		plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT 2")))

	c := tr.last(t)
	require.False(t, c.closed, "clean connections must return to the pool")
	require.Equal(t, 0, c.resets, "returning a clean connection must not reset it")
	require.Equal(t, 1, tr.opens(), "the second query must reuse the connection")
}

func TestPipeline_UndrainedStreamDiscards(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
		c.undrained = true
	}))), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	require.Empty(t, rt.leases)
	require.True(t, tr.last(t).closed, "unread rows make the connection unsafe to reuse")
}

func TestPipeline_ForwardFailureDiscards(t *testing.T) {
	tr := &tracker{}
	p, err := New(clusterOf(t, sliceWith("default", newPool(t, "master", tr, func(c *fakeConn) {
		c.forwardErr = errors.New("forward failed")
	}))), plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	require.Error(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	require.Empty(t, rt.leases)
	require.True(t, tr.last(t).closed, "a broken forward must discard the connection")
}
