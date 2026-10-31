package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/acorndb/prism/pkg/backend"
	"github.com/acorndb/prism/pkg/constant"
	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/session"
	"github.com/acorndb/prism/pkg/sql"
	"github.com/stretchr/testify/require"
)

func stmt(raw string) *sql.Statement {
	return &sql.Statement{Cmd: mysql.ComQuery, Raw: raw}
}

type recordingConn struct {
	resets int
	closed bool
	tag    string
}

func (b *recordingConn) Reset(context.Context) error {
	b.resets++
	return nil
}

func (b *recordingConn) Close() error {
	b.closed = true
	return nil
}

func (b *recordingConn) Align(context.Context, string, string) error { return nil }

func newTaggedPool(t *testing.T, tag string, opens *int) *backend.Pool {
	t.Helper()
	p := backend.NewPool(backend.PoolConfig{Capacity: 1, MaxCapacity: 4, GetTimeout: 0},
		func(context.Context) (backend.Conn, error) {
			if opens != nil {
				*opens++
			}
			return &recordingConn{tag: tag}, nil
		})
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestNew_Modes(t *testing.T) {
	g := &backend.Slice{Master: newTaggedPool(t, "m", nil)}
	e, err := New(constant.ModeSingle, g)
	require.NoError(t, err)
	require.Equal(t, constant.ModeSingle, e.Mode())

	e, err = New(constant.ModeRWSplit, g)
	require.NoError(t, err)
	require.Equal(t, constant.ModeRWSplit, e.Mode())

	e, err = New(constant.ModeSharding, g)
	require.NoError(t, err)
	require.Equal(t, constant.ModeSharding, e.Mode())

	_, err = New("nope", g)
	require.Error(t, err)
}

func TestSharding_HandleUnimplemented(t *testing.T) {
	e, err := New(constant.ModeSharding, &backend.Slice{})
	require.NoError(t, err)
	err = e.Handle(context.Background(), &Conn{State: session.NewState()}, stmt("SELECT 1"))
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrShardingNotImplemented))
}

func TestSingle_SelectUsesMasterOnly(t *testing.T) {
	var masterOpens, slaveOpens int
	g := &backend.Slice{
		Master: newTaggedPool(t, "master", &masterOpens),
		Slaves: []*backend.Pool{newTaggedPool(t, "slave", &slaveOpens)},
	}
	e, err := New(constant.ModeSingle, g)
	require.NoError(t, err)
	pt := e.(*passthrough)

	c := &Conn{State: session.NewState()}
	var gotTag string
	require.NoError(t, pt.withBackend(context.Background(), c, stmt("SELECT 1"), func(b backend.Conn) error {
		gotTag = b.(*recordingConn).tag
		return nil
	}))
	require.Equal(t, "master", gotTag)
	require.Equal(t, 1, masterOpens)
	require.Equal(t, 0, slaveOpens)
	require.Nil(t, c.Sticky)
}

func TestRWSplit_SelectUsesSlave(t *testing.T) {
	var masterOpens, slaveOpens int
	g := &backend.Slice{
		Master: newTaggedPool(t, "master", &masterOpens),
		Slaves: []*backend.Pool{newTaggedPool(t, "slave", &slaveOpens)},
	}
	e, err := New(constant.ModeRWSplit, g)
	require.NoError(t, err)
	pt := e.(*passthrough)

	c := &Conn{State: session.NewState()}
	var gotTag string
	require.NoError(t, pt.withBackend(context.Background(), c, stmt("SELECT 1"), func(b backend.Conn) error {
		gotTag = b.(*recordingConn).tag
		return nil
	}))
	require.Equal(t, "slave", gotTag)
	require.Equal(t, 0, masterOpens)
	require.Equal(t, 1, slaveOpens)
}

// P7 stub smoke: prove route to slave vs master without real MySQL (design rwsplit-route-smoke).
func TestRWSplit_RouteSmoke(t *testing.T) {
	cases := []struct {
		name        string
		sql         string
		wantTag     string
		setupSticky bool // BEGIN+InTx before the statement
	}{
		{name: "select_slave", sql: "SELECT 1", wantTag: "slave"},
		{name: "show_slave", sql: "SHOW TABLES", wantTag: "slave"},
		{name: "explain_slave", sql: "EXPLAIN SELECT 1", wantTag: "slave"},
		{name: "insert_master", sql: "INSERT INTO t VALUES (1)", wantTag: "master"},
		{name: "update_master", sql: "UPDATE t SET a=1", wantTag: "master"},
		{name: "delete_master", sql: "DELETE FROM t", wantTag: "master"},
		{name: "set_master", sql: "SET NAMES utf8mb4", wantTag: "master"},
		{name: "begin_master", sql: "BEGIN", wantTag: "master"},
		{name: "select_for_update_master", sql: "SELECT id FROM t FOR UPDATE", wantTag: "master"},
		{name: "tx_select_sticky_master", sql: "SELECT 1", wantTag: "master", setupSticky: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var masterOpens, slaveOpens int
			g := &backend.Slice{
				Master: newTaggedPool(t, "master", &masterOpens),
				Slaves: []*backend.Pool{newTaggedPool(t, "slave", &slaveOpens)},
			}
			e, err := New(constant.ModeRWSplit, g)
			require.NoError(t, err)
			pt := e.(*passthrough)
			c := &Conn{State: session.NewState()}
			ctx := context.Background()

			if tc.setupSticky {
				require.NoError(t, pt.withBackend(ctx, c, stmt("BEGIN"), func(backend.Conn) error {
					c.State.SetInTx(true)
					return nil
				}))
				require.Equal(t, 1, masterOpens)
				masterOpens, slaveOpens = 0, 0 // focus on the follow-up statement's new opens
			}

			var gotTag string
			require.NoError(t, pt.withBackend(ctx, c, stmt(tc.sql), func(b backend.Conn) error {
				gotTag = b.(*recordingConn).tag
				return nil
			}))
			require.Equal(t, tc.wantTag, gotTag)
			if tc.setupSticky {
				require.Equal(t, 0, masterOpens+slaveOpens, "sticky must not open a new pool conn")
			}
		})
	}
}

func TestRWSplit_TxSelectStaysSticky(t *testing.T) {
	var masterOpens, slaveOpens int
	g := &backend.Slice{
		Master: newTaggedPool(t, "master", &masterOpens),
		Slaves: []*backend.Pool{newTaggedPool(t, "slave", &slaveOpens)},
	}
	e, err := New(constant.ModeRWSplit, g)
	require.NoError(t, err)
	pt := e.(*passthrough)
	c := &Conn{State: session.NewState()}

	require.NoError(t, pt.withBackend(context.Background(), c, stmt("BEGIN"), func(backend.Conn) error {
		c.State.SetInTx(true)
		return nil
	}))
	require.NotNil(t, c.Sticky)
	require.Equal(t, 1, masterOpens)
	require.Equal(t, 0, slaveOpens)

	require.NoError(t, pt.withBackend(context.Background(), c, stmt("SELECT 1"), func(backend.Conn) error {
		return nil
	}))
	require.Equal(t, 1, masterOpens)
	require.Equal(t, 0, slaveOpens)
	require.NotNil(t, c.Sticky)
}

func TestReleaseSticky(t *testing.T) {
	fb := &recordingConn{tag: "sticky"}
	p := backend.NewPool(backend.PoolConfig{Capacity: 1, MaxCapacity: 2},
		func(context.Context) (backend.Conn, error) {
			t.Fatal("should not open")
			return nil, nil
		})
	t.Cleanup(func() { _ = p.Close() })
	e, err := New(constant.ModeSingle, &backend.Slice{Master: p})
	require.NoError(t, err)

	c := &Conn{State: session.NewState(), Sticky: fb, StickyPool: p}
	c.State.SetInTx(true)
	require.NoError(t, e.ReleaseSticky(c, false))
	require.Nil(t, c.Sticky)
	require.Equal(t, 1, fb.resets)

	fb2 := &recordingConn{tag: "drop"}
	c2 := &Conn{Sticky: fb2, StickyPool: p}
	require.NoError(t, e.ReleaseSticky(c2, true))
	require.Nil(t, c2.Sticky)
	require.True(t, fb2.closed)
}
