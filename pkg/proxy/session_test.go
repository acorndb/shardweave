package proxy

import (
	"context"
	"testing"

	"github.com/acorndb/prism/pkg/pool"
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
	p := pool.New(pool.Config{Capacity: 1, MaxCapacity: 2, GetTimeout: 0},
		func(context.Context) (pool.Backend, error) {
			t.Fatal("should not open new backend")
			return nil, nil
		})
	t.Cleanup(func() { _ = p.Close() })

	st := session.NewState()
	st.SetDatabase("app")
	st.SetCharset("utf8mb4")
	st.SetInTx(true)
	st.MarkReserved()
	var sticky pool.Backend = fb

	require.NoError(t, handleComResetConnection(ctx, p, st, &sticky))

	require.Nil(t, sticky)
	require.Equal(t, 1, fb.resets, "Release should COM_RESET the sticky backend")
	require.False(t, fb.closed)
	require.Equal(t, "app", st.Database())
	require.False(t, st.InTx())
	require.False(t, st.Reserved())
	require.Equal(t, "", st.Charset())
}

func TestHandleComResetConnection_NoSticky(t *testing.T) {
	ctx := context.Background()
	p := pool.New(pool.Config{Capacity: 0, MaxCapacity: 1},
		func(context.Context) (pool.Backend, error) {
			t.Fatal("should not open")
			return nil, nil
		})
	st := session.NewState()
	st.SetDatabase("db")
	st.MarkReserved()
	var sticky pool.Backend

	require.NoError(t, handleComResetConnection(ctx, p, st, &sticky))
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
