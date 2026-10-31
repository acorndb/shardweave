// Package log wraps slog with a process-wide level that can change at runtime
// (config hot-reload), similar to dbpack/Gaea atomic log level.
package log

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/acorndb/shardweave/pkg/constant"
)

var (
	mu     sync.Mutex
	level  = &slog.LevelVar{}
	inited bool
)

// ParseLevel maps config strings to slog levels; unknown values become Info.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case constant.LogLevelDebug:
		return slog.LevelDebug
	case constant.LogLevelWarn, constant.LogLevelWarning:
		return slog.LevelWarn
	case constant.LogLevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Init installs the default text handler on stderr at the given level.
func Init(levelName string) {
	InitWithWriter(levelName, os.Stderr)
}

// InitWithWriter is like Init but writes to w (tests).
func InitWithWriter(levelName string, w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	level.Set(ParseLevel(levelName))
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(h))
	inited = true
}

// SetLevel updates the runtime log level without rebuilding the handler.
// Safe to call on SIGUSR1 config reload.
func SetLevel(levelName string) {
	mu.Lock()
	defer mu.Unlock()
	if !inited {
		level.Set(ParseLevel(levelName))
		h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
		slog.SetDefault(slog.New(h))
		inited = true
		return
	}
	level.Set(ParseLevel(levelName))
}

// Level returns the current slog level.
func Level() slog.Level {
	return level.Level()
}
