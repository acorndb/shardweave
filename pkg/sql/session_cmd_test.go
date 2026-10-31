package sql

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func boolPtr(v bool) *bool { return &v }

func TestParseUse(t *testing.T) {
	cases := []struct {
		sql string
		db  string
		ok  bool
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

func TestParseSetNames(t *testing.T) {
	cases := []struct {
		sql string
		cs  string
		ok  bool
	}{
		{"SET NAMES utf8mb4", "utf8mb4", true},
		{"set names utf8mb4;", "utf8mb4", true},
		{"SET NAMES `utf8mb4` COLLATE utf8mb4_bin", "utf8mb4", true},
		{"SET @x = 1", "", false},
		{"SELECT 1", "", false},
		{"SET NAMES", "", false},
	}
	for _, tc := range cases {
		cs, ok := parseSetNames(tc.sql)
		require.Equal(t, tc.ok, ok, tc.sql)
		require.Equal(t, tc.cs, cs, tc.sql)
	}
}

func TestParseSetAutocommit(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
		ok   bool
	}{
		{"SET autocommit=0", false, true},
		{"SET AUTOCOMMIT = 1", true, true},
		{"set session autocommit=off", false, true},
		{"SET @@autocommit=ON", true, true},
		{"SET NAMES utf8mb4", false, false},
		{"SELECT 1", false, false},
	}
	for _, tc := range cases {
		v, ok := parseSetAutocommit(tc.sql)
		require.Equal(t, tc.ok, ok, tc.sql)
		if tc.ok {
			require.Equal(t, tc.want, v, tc.sql)
		}
	}
}

func TestParseSessionEffect(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want Effect
	}{
		{name: "use", sql: "USE app", want: Effect{UseDB: "app"}},
		{name: "set_names", sql: "SET NAMES utf8mb4", want: Effect{Charset: "utf8mb4"}},
		{name: "autocommit_off", sql: "SET autocommit=0", want: Effect{Autocommit: boolPtr(false)}},
		{name: "autocommit_on", sql: "SET SESSION autocommit=ON", want: Effect{Autocommit: boolPtr(true)}},
		{name: "no_effect", sql: "SELECT 1", want: Effect{}},
		{name: "user_var", sql: "SET @x=1", want: Effect{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ParseSessionEffect(tc.sql))
		})
	}
}
