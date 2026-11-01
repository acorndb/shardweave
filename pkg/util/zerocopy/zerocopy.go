// Package zerocopy provides zero-copy views over byte slices.
//
// Results alias the source memory: callers must guarantee the source is not
// mutated for as long as the result is alive, and must clone (strings.Clone)
// anything retained beyond that window.
package zerocopy

import "unsafe"

// HackString returns b as a string without copying. It follows the hack.String
// convention used by pingcap/tidb.
func HackString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}
