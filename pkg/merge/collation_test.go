package merge_test

import (
	"testing"

	"github.com/acorndb/shardweave/pkg/merge"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/result"
	"github.com/stretchr/testify/require"
)

// MySQL collation IDs used by the D2 whitelist tests.
const (
	collUTF8mb4GeneralCI  uint16 = 45
	collUTF8mb4Bin        uint16 = 46
	collUTF8mb4030900AIci uint16 = 255
	collUTF8mb4UnicodeCI  uint16 = 224 // not in the whitelist
	collBinary            uint16 = 63
)

func strField(name string, collID uint16) result.Field {
	return result.Field{Name: name, OrgName: name, Type: typeVarStr, Charset: collID}
}

func strRows(vals ...string) []result.Row {
	out := make([]result.Row, 0, len(vals))
	for _, v := range vals {
		out = append(out, result.Row{[]byte(v)})
	}
	return out
}

func orderByCol0() []plan.OrderKey {
	return []plan.OrderKey{{Kind: plan.OrderByPosition, Column: 0}}
}

func TestOrdered_CollationGeneralCI(t *testing.T) {
	fields := []result.Field{strField("name", collUTF8mb4GeneralCI)}
	// Each shard returns rows sorted by utf8mb4_general_ci.
	s0 := &scriptStream{fields: fields, rows: strRows("A", "c")}
	s1 := &scriptStream{fields: fields, rows: strRows("b", "D")}

	out, err := merge.Ordered([]result.Stream{s0, s1}, fields, orderByCol0())
	require.NoError(t, err)
	require.Equal(t, [][]string{{"A"}, {"b"}, {"c"}, {"D"}}, collectValues(t, out),
		"general_ci is case-insensitive: A < b < c < D, unlike byte order")
}

func TestOrdered_CollationBin(t *testing.T) {
	fields := []result.Field{strField("name", collUTF8mb4Bin)}
	// utf8mb4_bin compares code points, which equals byte order for UTF-8.
	s0 := &scriptStream{fields: fields, rows: strRows("A", "b")}
	s1 := &scriptStream{fields: fields, rows: strRows("C", "a")}

	out, err := merge.Ordered([]result.Stream{s0, s1}, fields, orderByCol0())
	require.NoError(t, err)
	require.Equal(t, [][]string{{"A"}, {"C"}, {"a"}, {"b"}}, collectValues(t, out))
}

func TestOrdered_CollationBinaryCharset(t *testing.T) {
	fields := []result.Field{strField("blob_col", collBinary)}
	s0 := &scriptStream{fields: fields, rows: strRows("b")}
	s1 := &scriptStream{fields: fields, rows: strRows("A")}

	out, err := merge.Ordered([]result.Stream{s0, s1}, fields, orderByCol0())
	require.NoError(t, err)
	require.Equal(t, [][]string{{"A"}, {"b"}}, collectValues(t, out))
}

func TestOrdered_CollationNotWhitelisted(t *testing.T) {
	fields := []result.Field{strField("name", collUTF8mb4UnicodeCI)}
	s0 := &scriptStream{fields: fields, rows: strRows("a")}

	_, err := merge.Ordered([]result.Stream{s0}, fields, orderByCol0())
	require.Error(t, err, "non-whitelisted collations must stay rejected")
	require.Contains(t, err.Error(), "utf8mb4_unicode_ci")
}

func TestSort_CollationGeneralCI(t *testing.T) {
	fields := []result.Field{strField("name", collUTF8mb4GeneralCI)}
	up := &scriptStream{fields: fields, rows: strRows("b", "A", "c")}

	out, err := merge.Sort(up, fields, orderByCol0())
	require.NoError(t, err)
	require.Equal(t, [][]string{{"A"}, {"b"}, {"c"}}, collectValues(t, out))
}

func TestOrdered_CollationZeroCharsetRejected(t *testing.T) {
	fields := []result.Field{{Name: "name", OrgName: "name", Type: typeVarStr, Charset: 0}}
	s0 := &scriptStream{fields: fields, rows: strRows("a")}

	_, err := merge.Ordered([]result.Stream{s0}, fields, orderByCol0())
	require.Error(t, err, "unknown collation id must be rejected")
}

func TestGroup_CollationWeightKey(t *testing.T) {
	fields := []result.Field{
		strField("name", collUTF8mb4GeneralCI),
		numField("cnt"),
	}
	specs := []plan.AggregateSpec{{Func: plan.AggCount, Column: 1, Output: 1}}
	// 'A' from shard 0 and 'a' from shard 1 are equal under general_ci and must
	// land in the same group; byte-order keys would split them.
	s0 := &scriptStream{fields: fields, rows: []result.Row{{[]byte("A"), []byte("1")}}}
	s1 := &scriptStream{fields: fields, rows: []result.Row{{[]byte("a"), []byte("1")}}}

	out, err := merge.Group([]result.Stream{s0, s1}, fields, []int{0}, specs, []int{0, 1})
	require.NoError(t, err)
	require.Equal(t, [][]string{{"A", "2"}}, collectValues(t, out),
		"collation-equal keys must merge into one group")
}

func TestDistinct_CollationWeightKey(t *testing.T) {
	fields := []result.Field{strField("name", collUTF8mb4GeneralCI)}
	s0 := &scriptStream{fields: fields, rows: strRows("A")}
	s1 := &scriptStream{fields: fields, rows: strRows("a")}

	out, err := merge.Distinct([]result.Stream{s0, s1}, fields)
	require.NoError(t, err)
	require.Equal(t, [][]string{{"A"}}, collectValues(t, out),
		"collation-equal rows must deduplicate across shards")
}

func TestAggregate_MinMaxCollation(t *testing.T) {
	fields := []result.Field{strField("name", collUTF8mb4GeneralCI)}
	specs := []plan.AggregateSpec{{Func: plan.AggMin, Column: 0, Output: 0}}
	// general_ci: 'a' < 'B'; byte order would pick 'B' (0x42 < 0x61).
	s0 := &scriptStream{fields: fields, rows: strRows("a")}
	s1 := &scriptStream{fields: fields, rows: strRows("B")}

	out, err := merge.Aggregate([]result.Stream{s0, s1}, fields, specs)
	require.NoError(t, err)
	rows := collectValues(t, out)
	require.Len(t, rows, 1)
	require.Equal(t, "a", rows[0][0], "MIN must use the column collation")
}

// MySQL PAD SPACE semantics: legacy collations ignore trailing spaces, so
// 'a' and 'a ' are one group / one distinct row. Verified against MySQL 8.0.
func TestGroup_CollationPadSpace(t *testing.T) {
	cases := []struct {
		name   string
		collID uint16
		want   [][]string
	}{
		{"general_ci_pad", collUTF8mb4GeneralCI, [][]string{{"a", "2"}}},
		{"bin_pad", collUTF8mb4Bin, [][]string{{"a", "2"}}},
		{"0900_no_pad", collUTF8mb4030900AIci, [][]string{{"a", "1"}, {"a ", "1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := []result.Field{strField("name", tc.collID), numField("cnt")}
			specs := []plan.AggregateSpec{{Func: plan.AggCount, Column: 1, Output: 1}}
			s0 := &scriptStream{fields: fields, rows: []result.Row{{[]byte("a"), []byte("1")}}}
			s1 := &scriptStream{fields: fields, rows: []result.Row{{[]byte("a "), []byte("1")}}}

			out, err := merge.Group([]result.Stream{s0, s1}, fields, []int{0}, specs, []int{0, 1})
			require.NoError(t, err)
			require.Equal(t, tc.want, collectValues(t, out))
		})
	}
}

func TestDistinct_CollationPadSpace(t *testing.T) {
	cases := []struct {
		name   string
		collID uint16
		want   [][]string
	}{
		{"general_ci_pad", collUTF8mb4GeneralCI, [][]string{{"a"}}},
		{"0900_no_pad", collUTF8mb4030900AIci, [][]string{{"a"}, {"a "}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := []result.Field{strField("name", tc.collID)}
			s0 := &scriptStream{fields: fields, rows: strRows("a")}
			s1 := &scriptStream{fields: fields, rows: strRows("a ")}

			out, err := merge.Distinct([]result.Stream{s0, s1}, fields)
			require.NoError(t, err)
			require.Equal(t, tc.want, collectValues(t, out))
		})
	}
}
