package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseUse(t *testing.T) {
	cases := []struct {
		sql  string
		db   string
		ok   bool
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
