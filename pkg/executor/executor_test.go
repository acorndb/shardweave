package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/acorndb/prism/pkg/backend_server"
	"github.com/acorndb/prism/pkg/config"
	"github.com/acorndb/prism/pkg/mysql"
	"github.com/acorndb/prism/pkg/router"
	"github.com/acorndb/prism/pkg/session"
	"github.com/stretchr/testify/require"
)

type recordingBackend struct {
	resets int
	closed bool
	tag    string
}

func (b *recordingBackend) Reset(context.Context) error {
	b.resets++
	return nil
}

func (b *recordingBackend) Close() error {
	b.closed = true
	return nil
}

func (b *recordingBackend) Align(context.Context, string, string) error { return nil }

func newTaggedPool(t *testing.T, tag string, opens *int) *backend_server.Pool {
	t.Helper()
	p := backend_server.New(backend_server.Config{Capacity: 1, MaxCapacity: 4, GetTimeout: 0},
		func(context.Context) (backend_server.Backend, error) {
			if opens != nil {
				*opens++
			}
			return &recordingBackend{tag: tag}, nil
		})
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestNew_Modes(t *testing.T) {
	g := &router.Group{Master: newTaggedPool(t, "m", nil)}
	e, err := New(config.ModeSingle, g)
	require.NoError(t, err)
	require.Equal(t, config.ModeSingle, e.Mode())

	e, err = New(config.ModeRWSplit, g)
	require.NoError(t, err)
	require.Equal(t, config.ModeRWSplit, e.Mode())

	e, err = New(config.ModeSharding, g)
	require.NoError(t, err)
	require.Equal(t, config.ModeSharding, e.Mode())

	_, err = New("nope", g)
	require.Error(t, err)
}

func TestSharding_HandleUnimplemented(t *testing.T) {
	e, err := New(config.ModeSharding, &router.Group{})
	require.NoError(t, err)
	err = e.Handle(context.Background(), &Conn{State: session.NewState()}, Request{Cmd: mysql.ComQuery, SQL: "SELECT 1"})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrShardingNotImplemented))
}

func TestSingle_SelectUsesMasterOnly(t *testing.T) {
	var masterOpens, slaveOpens int
	g := &router.Group{
		Master: newTaggedPool(t, "master", &masterOpens),
		Slaves: []*backend_server.Pool{newTaggedPool(t, "slave", &slaveOpens)},
	}
	e, err := New(config.ModeSingle, g)
	require.NoError(t, err)
	pt := e.(*passthrough)

	c := &Conn{State: session.NewState()}
	var gotTag string
	require.NoError(t, pt.withBackend(context.Background(), c, Request{Cmd: mysql.ComQuery, SQL: "SELECT 1"}, func(b backend_server.Backend) error {
		gotTag = b.(*recordingBackend).tag
		return nil
	}))
	require.Equal(t, "master", gotTag)
	require.Equal(t, 1, masterOpens)
	require.Equal(t, 0, slaveOpens)
	require.Nil(t, c.Sticky)
}

func TestRWSplit_SelectUsesSlave(t *testing.T) {
	var masterOpens, slaveOpens int
	g := &router.Group{
		Master: newTaggedPool(t, "master", &masterOpens),
		Slaves: []*backend_server.Pool{newTaggedPool(t, "slave", &slaveOpens)},
	}
	e, err := New(config.ModeRWSplit, g)
	require.NoError(t, err)
	pt := e.(*passthrough)

	c := &Conn{State: session.NewState()}
	var gotTag string
	require.NoError(t, pt.withBackend(context.Background(), c, Request{Cmd: mysql.ComQuery, SQL: "SELECT 1"}, func(b backend_server.Backend) error {
		gotTag = b.(*recordingBackend).tag
		return nil
	}))
	require.Equal(t, "slave", gotTag)
	require.Equal(t, 0, masterOpens)
	require.Equal(t, 1, slaveOpens)
}

func TestRWSplit_TxSelectStaysSticky(t *testing.T) {
	var masterOpens, slaveOpens int
	g := &router.Group{
		Master: newTaggedPool(t, "master", &masterOpens),
		Slaves: []*backend_server.Pool{newTaggedPool(t, "slave", &slaveOpens)},
	}
	e, err := New(config.ModeRWSplit, g)
	require.NoError(t, err)
	pt := e.(*passthrough)
	c := &Conn{State: session.NewState()}

	require.NoError(t, pt.withBackend(context.Background(), c, Request{Cmd: mysql.ComQuery, SQL: "BEGIN"}, func(backend_server.Backend) error {
		c.State.SetInTx(true)
		return nil
	}))
	require.NotNil(t, c.Sticky)
	require.Equal(t, 1, masterOpens)
	require.Equal(t, 0, slaveOpens)

	require.NoError(t, pt.withBackend(context.Background(), c, Request{Cmd: mysql.ComQuery, SQL: "SELECT 1"}, func(backend_server.Backend) error {
		return nil
	}))
	require.Equal(t, 1, masterOpens)
	require.Equal(t, 0, slaveOpens)
	require.NotNil(t, c.Sticky)
}

func TestReleaseSticky(t *testing.T) {
	fb := &recordingBackend{tag: "sticky"}
	p := backend_server.New(backend_server.Config{Capacity: 1, MaxCapacity: 2},
		func(context.Context) (backend_server.Backend, error) {
			t.Fatal("should not open")
			return nil, nil
		})
	t.Cleanup(func() { _ = p.Close() })
	e, err := New(config.ModeSingle, &router.Group{Master: p})
	require.NoError(t, err)

	c := &Conn{State: session.NewState(), Sticky: fb, StickyPool: p}
	c.State.SetInTx(true)
	require.NoError(t, e.ReleaseSticky(c, false))
	require.Nil(t, c.Sticky)
	require.Equal(t, 1, fb.resets)

	fb2 := &recordingBackend{tag: "drop"}
	c2 := &Conn{Sticky: fb2, StickyPool: p}
	require.NoError(t, e.ReleaseSticky(c2, true))
	require.Nil(t, c2.Sticky)
	require.True(t, fb2.closed)
}
