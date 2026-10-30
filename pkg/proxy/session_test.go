package proxy

import (
	"context"
	"testing"

	"github.com/acorndb/prism/pkg/backend_server"
	"github.com/acorndb/prism/pkg/router"
	"github.com/acorndb/prism/pkg/session"
	"github.com/stretchr/testify/require"
)

type recordingBackend struct {
	resets int
	closed bool
}

func (b *recordingBackend) Reset(context.Context) error {
	b.resets++
	return nil
}

func (b *recordingBackend) Close() error {
	b.closed = true
	return nil
}

func TestHandleComResetConnection(t *testing.T) {
	ctx := context.Background()
	fb := &recordingBackend{}
	p := backend_server.New(backend_server.Config{Capacity: 1, MaxCapacity: 2, GetTimeout: 0},
		func(context.Context) (backend_server.Backend, error) {
			t.Fatal("should not open new backend")
			return nil, nil
		})
	t.Cleanup(func() { _ = p.Close() })
	g := &router.Group{Master: p}

	st := session.NewState()
	st.SetDatabase("app")
	st.SetCharset("utf8mb4")
	st.SetInTx(true)
	st.MarkReserved()
	var sticky backend_server.Backend = fb
	stickyPool := p

	require.NoError(t, handleComResetConnection(ctx, g, st, &sticky, &stickyPool))

	require.Nil(t, sticky)
	require.Nil(t, stickyPool)
	require.Equal(t, 1, fb.resets, "Release should COM_RESET the sticky backend")
	require.False(t, fb.closed)
	require.Equal(t, "app", st.Database())
	require.False(t, st.InTx())
	require.False(t, st.Reserved())
	require.Equal(t, "", st.Charset())
	require.True(t, st.Autocommit())
}

func TestHandleComResetConnection_NoSticky(t *testing.T) {
	ctx := context.Background()
	p := backend_server.New(backend_server.Config{Capacity: 0, MaxCapacity: 1},
		func(context.Context) (backend_server.Backend, error) {
			t.Fatal("should not open")
			return nil, nil
		})
	g := &router.Group{Master: p}
	st := session.NewState()
	st.SetDatabase("db")
	st.MarkReserved()
	var sticky backend_server.Backend
	var stickyPool *backend_server.Pool

	require.NoError(t, handleComResetConnection(ctx, g, st, &sticky, &stickyPool))
	require.Equal(t, "db", st.Database())
	require.False(t, st.Reserved())
}

func TestParseUse(t *testing.T) {
	cases := []struct {
		sql string
		db  string
		ok  bool
	}{
		{"USE foo", "foo", true},
		{"use `bar`", "bar", true},
		{"USE \"baz\";", "baz", true},
		{"USE 'qux'", "qux", true},
		{"SELECT 1", "", false},
		{"USE", "", false},
		{"  use   db1  ", "db1", true},
	}
	for _, tc := range cases {
		db, ok := parseUse(tc.sql)
		require.Equal(t, tc.ok, ok, tc.sql)
		require.Equal(t, tc.db, db, tc.sql)
	}
}

func TestParseSetNames(t *testing.T) {
	cases := []struct {
		sql string
		cs  string
		ok  bool
	}{
		{"SET NAMES utf8mb4", "utf8mb4", true},
		{"set names utf8mb4;", "utf8mb4", true},
		{"SET NAMES `utf8mb4` COLLATE utf8mb4_bin", "utf8mb4", true},
		{"SET @x = 1", "", false},
		{"SELECT 1", "", false},
		{"SET NAMES", "", false},
	}
	for _, tc := range cases {
		cs, ok := parseSetNames(tc.sql)
		require.Equal(t, tc.ok, ok, tc.sql)
		require.Equal(t, tc.cs, cs, tc.sql)
	}
}

func TestParseSetAutocommit(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
		ok   bool
	}{
		{"SET autocommit=0", false, true},
		{"SET AUTOCOMMIT = 1", true, true},
		{"set session autocommit=off", false, true},
		{"SET @@autocommit=ON", true, true},
		{"SET NAMES utf8mb4", false, false},
		{"SELECT 1", false, false},
	}
	for _, tc := range cases {
		v, ok := parseSetAutocommit(tc.sql)
		require.Equal(t, tc.ok, ok, tc.sql)
		if tc.ok {
			require.Equal(t, tc.want, v, tc.sql)
		}
	}
}

func TestWithBackend_TxSelectStaysOnStickyPool(t *testing.T) {
	ctx := context.Background()
	var masterOpens, slaveOpens int
	master := backend_server.New(backend_server.Config{Capacity: 1, MaxCapacity: 2, GetTimeout: 0},
		func(context.Context) (backend_server.Backend, error) {
			masterOpens++
			return &alignRecordingBackend{}, nil
		})
	slave := backend_server.New(backend_server.Config{Capacity: 1, MaxCapacity: 2, GetTimeout: 0},
		func(context.Context) (backend_server.Backend, error) {
			slaveOpens++
			return &alignRecordingBackend{}, nil
		})
	t.Cleanup(func() {
		_ = master.Close()
		_ = slave.Close()
	})
	g := &router.Group{Master: master, Slaves: []*backend_server.Pool{slave}}

	st := session.NewState()
	var sticky backend_server.Backend
	var stickyPool *backend_server.Pool

	// BEGIN → master + sticky
	require.NoError(t, withBackend(ctx, g, st, 0x03, "BEGIN", &sticky, &stickyPool, func(b backend_server.Backend) error {
		st.SetInTx(true)
		return nil
	}))
	require.NotNil(t, sticky)
	require.Equal(t, 1, masterOpens)
	require.Equal(t, 0, slaveOpens)

	// SELECT in tx must not open slave
	require.NoError(t, withBackend(ctx, g, st, 0x03, "SELECT 1", &sticky, &stickyPool, func(b backend_server.Backend) error {
		return nil
	}))
	require.Equal(t, 1, masterOpens)
	require.Equal(t, 0, slaveOpens)
	require.NotNil(t, sticky)
}

func TestAlignBackend_CallsAligner(t *testing.T) {
	st := session.NewState()
	st.SetDatabase("app")
	st.SetCharset("utf8mb4")
	fb := &alignRecordingBackend{}
	require.NoError(t, alignBackend(context.Background(), fb, st))
	require.Equal(t, "app", fb.db)
	require.Equal(t, "utf8mb4", fb.cs)
}

type alignRecordingBackend struct {
	recordingBackend
	db, cs string
}

func (b *alignRecordingBackend) Align(_ context.Context, database, charset string) error {
	b.db = database
	b.cs = charset
	return nil
}
