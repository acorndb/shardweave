package sql_test

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/sql"
	"github.com/stretchr/testify/require"
)

func TestStatement_Contract(t *testing.T) {
	payload := []byte{0x03, 'S', 'E', 'L', 'E', 'C', 'T', ' ', '1'}

	st := &sql.Statement{
		Cmd:     payload[0],
		Payload: payload,
		Raw:     string(payload[1:]),
		Kind:    sql.KindQuery,
	}

	require.Equal(t, byte(0x03), st.Cmd)
	require.Equal(t, "SELECT 1", st.Raw)
	require.Equal(t, sql.KindQuery, st.Kind)
	require.Equal(t, payload, st.Payload, "payload must be forwarded byte-for-byte")
}
