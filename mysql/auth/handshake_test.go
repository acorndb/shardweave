package auth_test

import (
	"testing"

	"github.com/acorndb/prism/mysql"
	"github.com/acorndb/prism/mysql/auth"
	"github.com/stretchr/testify/require"
)

func TestHandshakeV10_RoundTrip(t *testing.T) {
	h := &auth.HandshakeV10{
		ProtocolVersion: 10,
		ServerVersion:   "8.0.36",
		ConnectionID:    42,
		AuthPluginData:  make([]byte, 20),
		Capability: mysql.ClientProtocol41 | mysql.ClientSecureConnection |
			mysql.ClientPluginAuth | mysql.ClientTransactions,
		Collation:      255,
		Status:         2, // SERVER_STATUS_AUTOCOMMIT
		AuthPluginName: auth.NativePasswordPlugin,
	}
	for i := range h.AuthPluginData {
		h.AuthPluginData[i] = byte(i + 1)
	}

	payload, err := h.Encode()
	require.NoError(t, err)

	got, err := auth.ParseHandshakeV10(payload)
	require.NoError(t, err)
	require.Equal(t, h.ProtocolVersion, got.ProtocolVersion)
	require.Equal(t, h.ServerVersion, got.ServerVersion)
	require.Equal(t, h.ConnectionID, got.ConnectionID)
	require.Equal(t, h.AuthPluginData, got.AuthPluginData)
	require.Equal(t, h.Capability, got.Capability)
	require.Equal(t, h.Collation, got.Collation)
	require.Equal(t, h.Status, got.Status)
	require.Equal(t, h.AuthPluginName, got.AuthPluginName)
}

func TestHandshakeResponse41_RoundTrip(t *testing.T) {
	serverCap := mysql.ClientProtocol41 | mysql.ClientSecureConnection |
		mysql.ClientPluginAuth | mysql.ClientPluginAuthLenEncClientData |
		mysql.ClientConnectWithDB

	r := &auth.HandshakeResponse41{
		Capability:     serverCap,
		MaxPacketSize:  0,
		Collation:      255,
		Username:       "root",
		AuthResponse:   auth.NativePassword("secret", make([]byte, 20)),
		Database:       "testdb",
		AuthPluginName: auth.NativePasswordPlugin,
	}
	payload, err := r.Encode()
	require.NoError(t, err)

	got, err := auth.ParseHandshakeResponse41(payload, serverCap)
	require.NoError(t, err)
	require.Equal(t, r.Username, got.Username)
	require.Equal(t, r.Database, got.Database)
	require.Equal(t, r.AuthPluginName, got.AuthPluginName)
	require.Equal(t, r.AuthResponse, got.AuthResponse)
	require.Equal(t, r.Collation, got.Collation)
	require.Equal(t, r.Capability&serverCap, got.Capability&serverCap)
}
