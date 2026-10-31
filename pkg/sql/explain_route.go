package sql

import "strings"

// ParseExplainRoute recognizes `EXPLAIN ROUTE <sql>` and returns the inner SQL.
// The inner statement is parsed and routed like a normal query, but executed
// by the proxy's explain writer instead of a backend.
func ParseExplainRoute(raw string) (string, bool) {
	s := trimLeadingSQLComments(strings.TrimSpace(raw))
	upper := strings.ToUpper(s)
	if !hasWordPrefix(upper, "EXPLAIN") {
		return "", false
	}
	rest := strings.TrimSpace(s[len("EXPLAIN"):])
	if !hasWordPrefix(strings.ToUpper(rest), "ROUTE") {
		return "", false
	}
	inner := strings.TrimSpace(rest[len("ROUTE"):])
	if inner == "" {
		return "", false
	}
	return inner, true
}
