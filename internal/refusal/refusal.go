// Package refusal is how camp says no.
//
// A refusal is not an error string. It names the rule that fired, so the
// tool's own tests can assert on something stable, and it carries a
// message written for somebody who has not read the specification: what
// the path is, what is true on each side, which side matters, the exact
// command that repairs it, and whose move it is. Everything user-facing
// in camp goes through this type, which is what keeps that standard
// enforceable rather than aspirational.
//
// Refusals collect. A configuration with four problems reports four
// problems, because fixing them one run at a time is the same work spread
// over four rounds of surprise.
package refusal

import (
	"fmt"
	"strings"
)

// R is one refusal.
type R struct {
	// Rule is the short, stable identifier of the rule that fired --
	// "target-nested", "overlap", "source-symlink". Tests match on this;
	// people read Message.
	Rule string
	// Message is the whole explanation, in sentences.
	Message string
}

// Error lets a single refusal be returned as an error.
func (r R) Error() string { return r.Message }

// New builds a refusal from a format string.
func New(rule, format string, args ...any) R {
	return R{Rule: rule, Message: fmt.Sprintf(format, args...)}
}

// List is every refusal one check found.
type List []R

// Add appends a refusal built from a format string.
func (l *List) Add(rule, format string, args ...any) {
	*l = append(*l, New(rule, format, args...))
}

// Extend appends every refusal of another list.
func (l *List) Extend(other List) { *l = append(*l, other...) }

// Empty reports whether nothing was refused.
func (l List) Empty() bool { return len(l) == 0 }

// Rules returns the identifiers that fired, for tests and for status
// lines that have no room for the prose.
func (l List) Rules() []string {
	rules := make([]string, 0, len(l))
	for _, r := range l {
		rules = append(rules, r.Rule)
	}
	return rules
}

// Has reports whether a particular rule fired.
func (l List) Has(rule string) bool {
	for _, r := range l {
		if r.Rule == rule {
			return true
		}
	}
	return false
}

// Error renders every refusal, one paragraph each.
func (l List) Error() string {
	parts := make([]string, 0, len(l))
	for _, r := range l {
		parts = append(parts, r.Message)
	}
	return strings.Join(parts, "\n\n")
}

// Err returns the list as an error, or nil when nothing was refused, so
// that callers can write the ordinary Go shape.
func (l List) Err() error {
	if len(l) == 0 {
		return nil
	}
	return l
}
