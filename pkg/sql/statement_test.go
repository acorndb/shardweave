package sql_test

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/sql"
	"github.com/stretchr/testify/require"
)

func TestIsDML(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"INSERT INTO t VALUES (1)", true},
		{"update t set a=1", true},
		{"DELETE FROM t", true},
		{"REPLACE INTO t VALUES (1)", true},
		{"LOAD DATA INFILE 'x' INTO TABLE t", true},
		{"/* c */ insert into t values (1)", true},
		{"SELECT 1", false},
		{"WITH x AS (SELECT 1) SELECT * FROM x", false},
		{"CREATE TABLE t (id INT)", false},
		{"SET autocommit=1", false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			require.Equal(t, tc.want, sql.IsDML(tc.raw))
		})
	}
}

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
