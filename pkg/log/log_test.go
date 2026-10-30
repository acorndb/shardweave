package log

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"nope", slog.LevelInfo},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			require.Equal(t, tc.want, ParseLevel(tc.in))
		})
	}
}

func TestSetLevel_Dynamic(t *testing.T) {
	var buf bytes.Buffer
	InitWithWriter("error", &buf)

	slog.Info("hidden")
	require.NotContains(t, buf.String(), "hidden")

	SetLevel("debug")
	slog.Debug("visible")
	require.Contains(t, buf.String(), "visible")
}
