package execute

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/acorndb/shardweave/pkg/backend"
	"github.com/acorndb/shardweave/pkg/constant"
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
	alignErr     error
	execErr      error
	execCalls    int
	aligns       int
	resets       int
	closed       bool
	lastReq      backend.ExecRequest
	lastAlign    backend.AlignSettings
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
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &fakeStream{
		status:       status,
		serverErr:    serverErr,
		affectedRows: affected,
		lastInsertID: lastID,
		resultSet:    resultSet,
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
	closed       bool
}

func (s *fakeStream) ForwardTo(context.Context, *packet.Conn) (mysql.ResponseInfo, error) {
	return mysql.ResponseInfo{
		Status:       s.status,
		ServerErr:    s.serverErr,
		AffectedRows: s.affectedRows,
		LastInsertID: s.lastInsertID,
		ResultSet:    s.resultSet,
	}, nil
}

func (s *fakeStream) Fields() []result.Field { return nil }

func (s *fakeStream) Next() (result.Row, error) { return nil, io.EOF }

func (s *fakeStream) Close() error {
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
	require.Equal(t, 1, c.resets, "non-sticky must return the lease")
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
	require.Equal(t, 0, c.resets)

	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	require.Equal(t, 2, c.execCalls)
	require.Equal(t, 1, tr.opens(), "sticky must reuse the lease")

	require.NoError(t, p.Release(rt, false))
	require.Empty(t, rt.leases)
	require.Equal(t, 1, c.resets)
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
	require.Equal(t, 1, c.resets)
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
	require.Equal(t, 1, c.resets)
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
	require.Equal(t, 1, tr.last(t).resets)
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
	require.Equal(t, 0, tr.last(t).resets)
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
	var rej *plan.RejectError
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
	require.Equal(t, 1, tr0.last(t).resets)
	require.Equal(t, 1, tr1.last(t).resets)
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
