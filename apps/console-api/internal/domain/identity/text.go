package identity

import "strings"

// trimSpace is the single trimming rule for operator-supplied labels: plain
// ASCII-and-Unicode whitespace around the value goes, interior content stays
// untouched. The aggregates trim so that a name of " " can never masquerade
// as a real one, and so the persisted value is the trimmed form — the
// database layer must not re-decide what a label means.
func trimSpace(s string) string {
	return strings.TrimSpace(s)
}
