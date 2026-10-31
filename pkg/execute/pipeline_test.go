package execute

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/acorndb/prism/pkg/backend"
	"github.com/acorndb/prism/pkg/constant"
	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/mysql/packet"
	"github.com/acorndb/prism/pkg/plan"
	"github.com/acorndb/prism/pkg/result"
	"github.com/acorndb/prism/pkg/route"
	"github.com/acorndb/prism/pkg/route/source"
	"github.com/acorndb/prism/pkg/session"
	"github.com/acorndb/prism/pkg/sql"
	"github.com/stretchr/testify/require"
)

type fakeConn struct {
	tag string

	mu        sync.Mutex
	status    uint16
	alignErr  error
	execErr   error
	execCalls int
	aligns    int
	resets    int
	closed    bool
	lastReq   backend.ExecRequest
	lastAlign backend.AlignSettings
}

func (c *fakeConn) Exec(_ context.Context, req backend.ExecRequest) (result.Stream, error) {
	c.mu.Lock()
	c.execCalls++
	c.lastReq = req
	status, err := c.status, c.execErr
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &fakeStream{status: status}, nil
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
	status uint16
	closed bool
}

func (s *fakeStream) ForwardTo(context.Context, *packet.Conn) (uint16, error) {
	return s.status, nil
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
	p := backend.NewPool(backend.PoolConfig{Capacity: 1, MaxCapacity: 4, GetTimeout: time.Second},
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

func TestNew_Errors(t *testing.T) {
	_, err := New(nil, plan.Builder{Router: source.AllMaster{}})
	require.Error(t, err)

	_, err = New(&backend.Slice{}, plan.Builder{Router: source.AllMaster{}})
	require.Error(t, err)

	p := backend.NewPool(backend.PoolConfig{MaxCapacity: 1}, nil)
	t.Cleanup(func() { _ = p.Close() })
	_, err = New(&backend.Slice{Master: p}, plan.Builder{})
	require.Error(t, err)
}

func TestPipeline_SingleUsesMaster(t *testing.T) {
	tr := &tracker{}
	g := &backend.Slice{
		Master: newPool(t, "master", tr, nil),
		Slaves: []*backend.Pool{newPool(t, "slave", tr, nil)},
	}
	p, err := New(g, plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	c := tr.last(t)
	require.Equal(t, "master", c.tag)
	require.Equal(t, 1, c.execCalls)
	require.Equal(t, 1, c.resets, "non-sticky must return the lease")
	require.Nil(t, rt.lease)
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
			g := &backend.Slice{
				Master: newPool(t, "master", tr, nil),
				Slaves: []*backend.Pool{newPool(t, "slave", tr, nil)},
			}
			p, err := New(g, plan.Builder{Router: source.Pick{}})
			require.NoError(t, err)

			require.NoError(t, p.Handle(context.Background(), newRuntime(), query(tc.raw)))
			require.Equal(t, tc.want, tr.last(t).tag)
		})
	}
}

func TestPipeline_StickyKeepsLease(t *testing.T) {
	tr := &tracker{}
	g := &backend.Slice{Master: newPool(t, "master", tr, func(c *fakeConn) {
		c.status = mysql.ServerStatusAutocommit | mysql.ServerStatusInTrans
	})}
	p, err := New(g, plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	require.NoError(t, p.Handle(context.Background(), rt, query("BEGIN")))
	require.NotNil(t, rt.lease)
	require.True(t, rt.State.InTx())
	c := tr.last(t)
	require.Equal(t, 1, c.execCalls)
	require.Equal(t, 0, c.resets)

	require.NoError(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	require.Equal(t, 2, c.execCalls)
	require.Equal(t, 1, tr.opens(), "sticky must reuse the lease")

	require.NoError(t, p.Release(rt, false))
	require.Nil(t, rt.lease)
	require.Equal(t, 1, c.resets)
}

func TestPipeline_ReleaseDiscard(t *testing.T) {
	tr := &tracker{}
	g := &backend.Slice{Master: newPool(t, "master", tr, func(c *fakeConn) {
		c.status = mysql.ServerStatusAutocommit | mysql.ServerStatusInTrans
	})}
	p, err := New(g, plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	require.NoError(t, p.Handle(context.Background(), rt, query("BEGIN")))
	require.NoError(t, p.Release(rt, true))
	c := tr.last(t)
	require.True(t, c.closed)
	require.Equal(t, 0, c.resets)
}

func TestPipeline_AlignFailureReleases(t *testing.T) {
	tr := &tracker{}
	g := &backend.Slice{Master: newPool(t, "master", tr, func(c *fakeConn) {
		c.alignErr = errors.New("align failed")
	})}
	p, err := New(g, plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	require.Error(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	require.Nil(t, rt.lease)
	c := tr.last(t)
	require.Equal(t, 1, c.resets)
	require.Equal(t, 0, c.execCalls)
}

func TestPipeline_ExecFailureReleases(t *testing.T) {
	tr := &tracker{}
	g := &backend.Slice{Master: newPool(t, "master", tr, func(c *fakeConn) {
		c.execErr = errors.New("exec failed")
	})}
	p, err := New(g, plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	rt := newRuntime()
	require.Error(t, p.Handle(context.Background(), rt, query("SELECT 1")))
	require.Nil(t, rt.lease)
	c := tr.last(t)
	require.Equal(t, 1, c.execCalls)
	require.Equal(t, 1, c.resets)
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
			g := &backend.Slice{Master: newPool(t, "master", tr, func(c *fakeConn) {
				c.status = mysql.ServerStatusAutocommit | mysql.ServerStatusInTrans
			})}
			p, err := New(g, plan.Builder{Router: source.AllMaster{}})
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

func TestPipeline_RejectPlan(t *testing.T) {
	tr := &tracker{}
	g := &backend.Slice{Master: newPool(t, "master", tr, nil)}
	p, err := New(g, plan.Builder{Router: fakeRouter{targets: []route.Target{
		{Slice: "s0", Role: constant.RoleMaster},
		{Slice: "s1", Role: constant.RoleMaster},
	}}})
	require.NoError(t, err)

	err = p.Handle(context.Background(), newRuntime(), query("SELECT 1"))
	var rej *plan.RejectError
	require.ErrorAs(t, err, &rej)
	require.Equal(t, 0, tr.opens())
}

func TestPipeline_NilInputs(t *testing.T) {
	tr := &tracker{}
	g := &backend.Slice{Master: newPool(t, "master", tr, nil)}
	p, err := New(g, plan.Builder{Router: source.AllMaster{}})
	require.NoError(t, err)

	require.Error(t, p.Handle(context.Background(), nil, query("SELECT 1")))
	require.Error(t, p.Handle(context.Background(), newRuntime(), nil))
	require.NoError(t, p.Release(nil, false))
	require.NoError(t, p.Release(newRuntime(), true))
}
