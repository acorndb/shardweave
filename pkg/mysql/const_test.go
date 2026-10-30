package mysql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProtocolConstants(t *testing.T) {
	cases := []struct {
		name string
		got  uint64
		want uint64
	}{
		{"OKHeader", uint64(OKHeader), 0x00},
		{"ErrHeader", uint64(ErrHeader), 0xff},
		{"EOFHeader", uint64(EOFHeader), 0xfe},
		{"ComQuit", uint64(ComQuit), 0x01},
		{"ComInitDB", uint64(ComInitDB), 0x02},
		{"ComQuery", uint64(ComQuery), 0x03},
		{"ComPing", uint64(ComPing), 0x0e},
		{"ComStmtPrepare", uint64(ComStmtPrepare), 0x16},
		{"ComStmtExecute", uint64(ComStmtExecute), 0x17},
		{"ComStmtClose", uint64(ComStmtClose), 0x19},
		{"ComStmtReset", uint64(ComStmtReset), 0x1a},
		{"ComResetConnection", uint64(ComResetConnection), 0x1f},
		{"ClientProtocol41", uint64(ClientProtocol41), 1 << 9},
		{"ClientSecureConnection", uint64(ClientSecureConnection), 1 << 15},
		{"ClientPluginAuth", uint64(ClientPluginAuth), 1 << 19},
		{"ClientTransactions", uint64(ClientTransactions), 1 << 13},
		{"ClientMultiStatements", uint64(ClientMultiStatements), 1 << 16},
		{"ClientMultiResults", uint64(ClientMultiResults), 1 << 17},
		{"ClientPSMultiResults", uint64(ClientPSMultiResults), 1 << 18},
		{"ClientPluginAuthLenEncClientData", uint64(ClientPluginAuthLenEncClientData), 1 << 21},
		{"ClientConnectWithDB", uint64(ClientConnectWithDB), 1 << 3},
		{"ClientDeprecateEOF", uint64(ClientDeprecateEOF), 1 << 24},
		{"ClientSessionTrack", uint64(ClientSessionTrack), 1 << 23},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.got)
		})
	}
}
