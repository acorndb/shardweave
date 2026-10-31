package errs_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/stretchr/testify/require"
)

func TestMySQL_Error(t *testing.T) {
	require.Equal(t, "Error 1235 (42000): nope",
		errs.New(errs.ErNotSupportedYet, errs.StateSyntax, "nope").Error())
	require.Equal(t, "Error 7 (HY000): default state",
		errs.New(7, "", "default state").Error())

	var nilErr *errs.MySQL
	require.Equal(t, "", nilErr.Error())
}

func TestMySQL_Constructors(t *testing.T) {
	cases := []struct {
		name    string
		got     *errs.MySQL
		number  uint16
		state   string
		message string
	}{
		{"unsupported", errs.Unsupported("bad %s", "shape"), 1235, "42000", "bad shape"},
		{"unknown", errs.Unknown("multi-target plan not implemented"), 1105, "HY000", "multi-target plan not implemented"},
		{"unknown_command", errs.UnknownCommand(0x1f), 1047, "08S01", "command 31 not supported yet"},
		{"too_many_connections", errs.TooManyConnections(), 1040, "08004", "Too many connections"},
		{"packet_too_large", errs.PacketTooLarge(), 1153, "08S01", "Got a packet bigger than 'max_allowed_packet' bytes"},
		{"access_denied", errs.AccessDenied("app"), 1045, "28000", "Access denied for user 'app'"},
		{"bad_handshake", errs.BadHandshake(errors.New("eof")), 1043, "08S01", "Bad handshake: eof"},
		{"bad_handshake_nil_cause", errs.BadHandshake(nil), 1043, "08S01", "Bad handshake"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NotNil(t, tc.got)
			require.Equal(t, tc.number, tc.got.Number)
			require.Equal(t, tc.state, tc.got.SQLState)
			require.Equal(t, tc.message, tc.got.Message)
		})
	}
}

func TestSentinels(t *testing.T) {
	require.ErrorIs(t, fmt.Errorf("%w: too big", errs.ErrPacketTooLarge), errs.ErrPacketTooLarge)
	require.ErrorIs(t, fmt.Errorf("%w: full", errs.ErrPoolExhausted), errs.ErrPoolExhausted)
	require.ErrorIs(t, fmt.Errorf("%w: ok packet", errs.ErrNoResultSet), errs.ErrNoResultSet)
}
