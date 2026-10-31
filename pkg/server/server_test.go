package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acorndb/prism/pkg/config"
	"github.com/acorndb/prism/pkg/constant"
	"github.com/stretchr/testify/require"
)

func TestServer_RunExitsOnClose(t *testing.T) {
	cfg := minimalConfig("127.0.0.1:0")
	path := writeConfig(t, cfg)

	srv, err := New(cfg, path)
	require.NoError(t, err)
	require.NotEmpty(t, srv.Addr())

	done := make(chan struct{})
	go func() {
		_ = srv.Run()
		close(done)
	}()

	srv.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Close")
	}
}

func TestServer_UpdateConfigUsers(t *testing.T) {
	cfg := minimalConfig("127.0.0.1:0")
	path := writeConfig(t, cfg)
	srv, err := New(cfg, path)
	require.NoError(t, err)
	t.Cleanup(srv.Close)

	pass, ok := srv.Runtime().UserProvider().Password("app")
	require.True(t, ok)
	require.Equal(t, "p1", pass)

	next, err := config.LoadFile(path)
	require.NoError(t, err)
	next.Users = []config.User{{Name: "app", Password: "p2"}}
	require.NoError(t, srv.UpdateConfig(next))

	pass, ok = srv.Runtime().UserProvider().Password("app")
	require.True(t, ok)
	require.Equal(t, "p2", pass)
}

func TestServer_UpdateConfigRejectsListenChange(t *testing.T) {
	cfg := minimalConfig("127.0.0.1:0")
	path := writeConfig(t, cfg)
	srv, err := New(cfg, path)
	require.NoError(t, err)
	t.Cleanup(srv.Close)

	next := cfg.Clone()
	next.Listen = "127.0.0.1:19999"
	err = srv.UpdateConfig(next)
	require.Error(t, err)
	require.Contains(t, err.Error(), "listen")
}

func minimalConfig(listen string) *config.Config {
	return &config.Config{
		Listen: listen,
		Users:  []config.User{{Name: "app", Password: "p1"}},
		BackendServer: config.BackendServer{
			Mode:     constant.ModeSingle,
			Address:  "127.0.0.1:3306",
			User:     "app",
			Password: "p1",
		},
		Log: config.Log{Level: "error"},
	}
}

func writeConfig(t *testing.T, cfg *config.Config) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prism.yaml")
	body := "listen: \"" + cfg.Listen + "\"\n" +
		"users:\n  - name: app\n    password: p1\n" +
		"backend_server:\n  mode: single\n  address: \"127.0.0.1:3306\"\n  user: app\n  password: p1\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}
