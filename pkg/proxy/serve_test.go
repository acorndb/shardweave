package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/acorndb/shardweave/pkg/execute"
	"github.com/acorndb/shardweave/pkg/mysql"
	"github.com/acorndb/shardweave/pkg/mysql/client"
	"github.com/acorndb/shardweave/pkg/mysql/packet"
	"github.com/acorndb/shardweave/pkg/session"
	"github.com/acorndb/shardweave/pkg/sql"
	"github.com/stretchr/testify/require"
)

type testUsers map[string]string

func (u testUsers) Password(user string) (string, bool) {
	p, ok := u[user]
	return p, ok
}

type handleCall struct {
	stmt       *sql.Statement
	db         string
	charset    string
	autocommit bool
}

type fakeExec struct {
	mu        sync.Mutex
	calls     []handleCall
	releases  []bool
	signal    chan struct{}
	handleErr error
	closed    bool
}

func newFakeExec() *fakeExec {
	return &fakeExec{signal: make(chan struct{}, 16)}
}

func (f *fakeExec) Handle(_ context.Context, c *execute.Runtime, stmt *sql.Statement) error {
	call := handleCall{
		stmt:       stmt,
		db:         c.State.Database(),
		charset:    c.State.Charset(),
		autocommit: c.State.Autocommit(),
	}
	if f.handleErr == nil {
		applyTestEffect(c.State, stmt.Effect)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	f.signal <- struct{}{}
	return f.handleErr
}

func applyTestEffect(st *session.State, e sql.Effect) {
	if e.UseDB != "" {
		st.SetDatabase(e.UseDB)
	}
	if e.Charset != "" {
		st.SetCharset(e.Charset)
	}
	if e.Autocommit != nil {
		st.SetAutocommit(*e.Autocommit)
	}
}

func (f *fakeExec) Release(_ *execute.Runtime, discard bool) error {
	f.mu.Lock()
	f.releases = append(f.releases, discard)
	f.mu.Unlock()
	return nil
}

func (f *fakeExec) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeExec) waitCall(t *testing.T) handleCall {
	t.Helper()
	select {
	case <-f.signal:
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.calls[len(f.calls)-1]
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for executor call")
		return handleCall{}
	}
}

func (f *fakeExec) lastRelease(t *testing.T) bool {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.releases, "expected a release call")
	return f.releases[len(f.releases)-1]
}

func newProxySession(t *testing.T, exec *fakeExec) (*packet.Conn, <-chan error) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	require.NoError(t, clientConn.SetDeadline(time.Now().Add(10*time.Second)))
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- Serve(ctx, serverConn, SessionConfig{
			Users: testUsers{"app": "pw"},
			Exec:  exec,
		})
	}()

	cli, err := client.Dial(clientConn, "app", "pw", "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.PacketConn().Close() })
	return cli.PacketConn(), serveErr
}

func writeCommand(t *testing.T, pc *packet.Conn, payload []byte) {
	t.Helper()
	pc.ResetSequence()
	buf := make([]byte, 4+len(payload))
	copy(buf[4:], payload)
	require.NoError(t, pc.WritePacket(buf))
}

func readPacket(t *testing.T, pc *packet.Conn) []byte {
	t.Helper()
	data, err := pc.ReadPacket()
	require.NoError(t, err)
	return data
}

func quit(t *testing.T, pc *packet.Conn, serveErr <-chan error) {
	t.Helper()
	writeCommand(t, pc, []byte{mysql.ComQuit})
	select {
	case err := <-serveErr:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after COM_QUIT")
	}
}

func TestServe_Ping(t *testing.T) {
	exec := newFakeExec()
	pc, serveErr := newProxySession(t, exec)

	writeCommand(t, pc, []byte{mysql.ComPing})
	data := readPacket(t, pc)
	require.NotEmpty(t, data)
	require.Equal(t, byte(mysql.OKHeader), data[0])

	quit(t, pc, serveErr)
}

func TestServe_ComQueryBuildsStatement(t *testing.T) {
	exec := newFakeExec()
	pc, serveErr := newProxySession(t, exec)

	payload := append([]byte{mysql.ComQuery}, []byte("SELECT 1")...)
	writeCommand(t, pc, payload)
	call := exec.waitCall(t)

	require.Equal(t, byte(mysql.ComQuery), call.stmt.Cmd)
	require.Equal(t, "SELECT 1", call.stmt.Raw)
	require.Equal(t, sql.KindQuery, call.stmt.Kind)
	require.Equal(t, payload, call.stmt.Payload)

	quit(t, pc, serveErr)
}

func TestServe_UseQueryUpdatesState(t *testing.T) {
	exec := newFakeExec()
	pc, serveErr := newProxySession(t, exec)

	writeCommand(t, pc, append([]byte{mysql.ComQuery}, []byte("USE app")...))
	call := exec.waitCall(t)
	require.Equal(t, "app", call.stmt.Effect.UseDB)
	require.Equal(t, "USE app", call.stmt.Raw)

	writeCommand(t, pc, append([]byte{mysql.ComQuery}, []byte("SELECT 1")...))
	next := exec.waitCall(t)
	require.Equal(t, "app", next.db, "USE effect must apply after success")

	quit(t, pc, serveErr)
}

func TestServe_SetAutocommitUpdatesState(t *testing.T) {
	exec := newFakeExec()
	pc, serveErr := newProxySession(t, exec)

	writeCommand(t, pc, append([]byte{mysql.ComQuery}, []byte("SET autocommit=0")...))
	call := exec.waitCall(t)
	require.NotNil(t, call.stmt.Effect.Autocommit)
	require.False(t, *call.stmt.Effect.Autocommit)
	require.Equal(t, "SET autocommit=0", call.stmt.Raw)

	writeCommand(t, pc, append([]byte{mysql.ComQuery}, []byte("SELECT 1")...))
	next := exec.waitCall(t)
	require.False(t, next.autocommit, "SET autocommit effect must apply after success")

	quit(t, pc, serveErr)
}

func TestServe_ComInitDB(t *testing.T) {
	exec := newFakeExec()
	pc, serveErr := newProxySession(t, exec)

	writeCommand(t, pc, append([]byte{mysql.ComInitDB}, []byte("app2")...))
	call := exec.waitCall(t)
	require.Equal(t, "app2", call.stmt.Effect.UseDB)
	require.Equal(t, "app2", call.stmt.Raw)
	require.Equal(t, sql.KindUse, call.stmt.Kind)

	writeCommand(t, pc, append([]byte{mysql.ComQuery}, []byte("SELECT 1")...))
	next := exec.waitCall(t)
	require.Equal(t, "app2", next.db, "COM_INIT_DB effect must apply after success")

	quit(t, pc, serveErr)
}

func TestServe_ComResetConnection(t *testing.T) {
	exec := newFakeExec()
	pc, serveErr := newProxySession(t, exec)

	writeCommand(t, pc, append([]byte{mysql.ComQuery}, []byte("SET autocommit=0")...))
	exec.waitCall(t)

	writeCommand(t, pc, []byte{mysql.ComResetConnection})
	data := readPacket(t, pc)
	require.Equal(t, byte(mysql.OKHeader), data[0])
	require.False(t, exec.lastRelease(t))

	writeCommand(t, pc, append([]byte{mysql.ComQuery}, []byte("SELECT 1")...))
	call := exec.waitCall(t)
	require.True(t, call.autocommit, "RESET must restore autocommit")

	quit(t, pc, serveErr)
}

func TestServe_UnknownCommandWritesERR(t *testing.T) {
	exec := newFakeExec()
	pc, serveErr := newProxySession(t, exec)

	writeCommand(t, pc, []byte{0x7f})
	data := readPacket(t, pc)
	require.NotEmpty(t, data)
	require.Equal(t, byte(mysql.ErrHeader), data[0])
	require.Equal(t, uint16(1047), binary.LittleEndian.Uint16(data[1:3]))

	quit(t, pc, serveErr)
}

func TestServe_HandleErrorClosesAndDiscards(t *testing.T) {
	exec := newFakeExec()
	exec.handleErr = errors.New("boom")
	pc, serveErr := newProxySession(t, exec)

	writeCommand(t, pc, append([]byte{mysql.ComQuery}, []byte("SELECT 1")...))
	select {
	case err := <-serveErr:
		require.Error(t, err)
		require.Contains(t, err.Error(), "boom")
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after executor error")
	}
	require.True(t, exec.lastRelease(t), "session close must discard the sticky backend")
}

func TestServe_PoolExhaustedWrites1040(t *testing.T) {
	exec := newFakeExec()
	exec.handleErr = execute.ErrPoolExhausted
	pc, serveErr := newProxySession(t, exec)

	writeCommand(t, pc, append([]byte{mysql.ComQuery}, []byte("SELECT 1")...))
	data := readPacket(t, pc)
	require.NotEmpty(t, data)
	require.Equal(t, byte(mysql.ErrHeader), data[0])
	require.Equal(t, uint16(1040), binary.LittleEndian.Uint16(data[1:3]))

	select {
	case err := <-serveErr:
		require.ErrorIs(t, err, execute.ErrPoolExhausted)
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not exit after pool exhaustion")
	}
}
