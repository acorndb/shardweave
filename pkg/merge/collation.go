package merge

import (
	"bytes"
	"strings"

	"github.com/acorndb/shardweave/pkg/errs"
	collation "github.com/acorndb/shardweave/pkg/mysql/collation"
	"github.com/acorndb/shardweave/pkg/mysql/collation/colldata"
	"github.com/acorndb/shardweave/pkg/result"
)

// collEnv is the process-wide MySQL 8 collation environment. Phase 1 assumes
// every slice runs the same server version (merge-engine-design §13.4).
var collEnv = collation.MySQL8()

// stringCollator is the subset of colldata.Collation the merge engine needs.
// A wrapper implements MySQL's PAD SPACE semantics on top of it.
type stringCollator interface {
	Collate(left, right []byte, rightIsPrefix bool) int
	WeightString(dst, src []byte, numCodepoints int) []byte
}

// paddedCollator ignores trailing spaces, matching MySQL's PAD SPACE
// collations (all legacy collations). Without this, GROUP BY/DISTINCT would
// split 'a' and 'a ' into different groups, unlike MySQL.
type paddedCollator struct {
	coll colldata.Collation
}

func (p paddedCollator) Collate(left, right []byte, rightIsPrefix bool) int {
	return p.coll.Collate(trimTrailingSpaces(left), trimTrailingSpaces(right), rightIsPrefix)
}

func (p paddedCollator) WeightString(dst, src []byte, numCodepoints int) []byte {
	return p.coll.WeightString(dst, trimTrailingSpaces(src), numCodepoints)
}

func trimTrailingSpaces(b []byte) []byte {
	return bytes.TrimRight(b, " ")
}

// byteCollator compares UTF-8 bytes (equal to code-point order) and optionally
// applies PAD SPACE by trimming trailing spaces. utf8mb4_bin is a PAD SPACE
// collation in MySQL, unlike the binary charset.
type byteCollator struct{ pad bool }

func (b byteCollator) Collate(left, right []byte, _ bool) int {
	if b.pad {
		left, right = trimTrailingSpaces(left), trimTrailingSpaces(right)
	}
	return bytes.Compare(left, right)
}

func (b byteCollator) WeightString(dst, src []byte, _ int) []byte {
	if b.pad {
		src = trimTrailingSpaces(src)
	}
	return append(dst, src...)
}

// collatorFor resolves the collation of one string column.
//
// Returns (nil, false, nil) for the binary charset (byte comparison, trailing
// bytes significant), (coll, true, nil) for a whitelisted collation, and an
// error otherwise.
//
// The whitelist is deliberately small: every admitted collation is a fidelity
// promise that must be covered by the metamorphic oracle (design §13.4).
func collatorFor(f result.Field) (stringCollator, bool, error) {
	id := collation.ID(f.Charset)
	if !collEnv.IsSupported(id) {
		return nil, false, errs.Unsupported(
			"multi-shard comparison on column %q: unknown collation id %d", f.Name, f.Charset)
	}
	name := collEnv.LookupName(id)
	switch name {
	case "binary":
		return nil, false, nil
	case "utf8mb4_bin":
		return byteCollator{pad: true}, true, nil
	case "utf8mb4_general_ci", "utf8mb4_0900_ai_ci":
		coll := colldata.Lookup(id)
		if coll == nil {
			return nil, false, errs.Unsupported(
				"multi-shard comparison on column %q: collation %q is unavailable", f.Name, name)
		}
		return wrapPadding(coll, name), true, nil
	default:
		return nil, false, errs.Unsupported(
			"multi-shard comparison on column %q: collation %q is not supported", f.Name, name)
	}
}

// wrapPadding applies MySQL's PAD SPACE rule: all collations except the UCA
// 9.0.0 family (`*_0900_*`, NO PAD) ignore trailing spaces in comparisons.
func wrapPadding(coll colldata.Collation, name string) stringCollator {
	if strings.Contains(name, "_0900_") {
		return coll
	}
	return paddedCollator{coll: coll}
}

// isStringType reports whether a result type carries collation semantics.
func isStringType(t byte) bool {
	switch t {
	case result.TypeVarchar, result.TypeVarString, result.TypeString,
		result.TypeEnum, result.TypeSet, result.TypeJSON,
		result.TypeTinyBlob, result.TypeBlob, result.TypeMediumBlob, result.TypeLongBlob:
		return true
	default:
		return false
	}
}

// orderComparatorFor resolves how one ORDER BY (or key/min/max) column is
// compared. Unlike comparatorFor (used by numeric/date keys), it admits
// whitelisted string collations.
func orderComparatorFor(f result.Field) (valueKind, stringCollator, error) {
	if !isStringType(f.Type) {
		kind, err := comparatorFor(f)
		return kind, nil, err
	}
	coll, collated, err := collatorFor(f)
	if err != nil {
		return 0, nil, err
	}
	if collated {
		return kindCollated, coll, nil
	}
	return kindText, nil, nil
}
