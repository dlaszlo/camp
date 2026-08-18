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

	// group and subjects are set for the rules that can fire many times in
	// one run, and they are what lets nine mounts failing one check become
	// one refusal naming nine paths. Unexported: grouping is this
	// package's own business, and nothing outside it should be able to
	// half-build a group by hand.
	group    *Group
	subjects []string
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

// Push appends a refusal that was built somewhere else, so that a check
// living in its own package still reports through the one mechanism.
func (l *List) Push(r R) { *l = append(*l, r) }

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

// Error renders every refusal, one paragraph each -- one per problem,
// with the subjects of a repeated one gathered onto it.
func (l List) Error() string {
	merged := l.Merge()
	parts := make([]string, 0, len(merged))
	for _, r := range merged {
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

// Group is one problem a check can meet several times in one run.
//
// A rule that fires once per mount, once per root entry or once per pair
// of names is not several problems. It is one problem with several
// subjects, and a reader handed nine copies of the same three paragraphs
// has to read all nine to find the nine paths in them. One paragraph and
// a list of nine is the same information and a fraction of the reading.
//
// So a rule that can fire more than once says itself in four parts: the
// opening for one subject, the opening for several, the explanation both
// share, and -- passed separately, at each site the rule fires -- the
// subject itself.
//
// Detail says nothing about any particular subject, and that is what
// makes the grouping work at all: two refusals merge when their rule and
// their explanation are identical, so a path or a name in the explanation
// would give every subject a group of its own. Everything specific to one
// subject belongs on its subject line.
type Group struct {
	// Rule is the identifier, as everywhere else.
	Rule string
	// One is the opening sentence when exactly one subject failed. It
	// takes no arguments: the subject is on its own line under it.
	One string
	// Many is the opening for several, taking the count.
	Many string
	// Detail is the explanation and the repair, printed once however many
	// subjects there are.
	Detail string
}

// Of builds the refusal of one subject.
//
// The subject is one line, formatted by the caller: the path, the name,
// the pair of paths, and whatever else belongs to this one instance --
// which side is which, what type each end is, which step declared it.
func Of(group Group, format string, args ...any) R {
	subjects := []string{fmt.Sprintf(format, args...)}
	return R{
		Rule:     group.Rule,
		Message:  compose(group, subjects),
		group:    &group,
		subjects: subjects,
	}
}

// Group appends the refusal of one subject.
func (l *List) Group(group Group, format string, args ...any) {
	*l = append(*l, Of(group, format, args...))
}

// compose renders a group's refusal for the subjects it fired for.
func compose(group Group, subjects []string) string {
	opening := group.One
	if len(subjects) != 1 {
		opening = fmt.Sprintf(group.Many, len(subjects))
	}
	parts := append([]string{opening}, subjects...)
	if group.Detail != "" {
		parts = append(parts, group.Detail)
	}
	return strings.Join(parts, "\n")
}

// Merge returns one refusal per problem: the refusals of one group, about
// several subjects, become one refusal naming all of them.
//
// Order is the order each problem was first met, so the reader still
// meets the checks in the order they ran. A refusal that belongs to no
// group passes through untouched, which is every rule that can only ever
// fire about one thing.
func (l List) Merge() List {
	merged := make(List, 0, len(l))
	where := map[string]int{}
	for _, item := range l {
		if item.group == nil {
			merged = append(merged, item)
			continue
		}
		key := strings.Join([]string{item.Rule, item.group.One, item.group.Many,
			item.group.Detail}, "\x00")
		index, seen := where[key]
		if !seen {
			where[key] = len(merged)
			item.subjects = append([]string(nil), item.subjects...)
			merged = append(merged, item)
			continue
		}
		merged[index].subjects = append(merged[index].subjects, item.subjects...)
		merged[index].Message = compose(*merged[index].group, merged[index].subjects)
	}
	return merged
}

// Count is how many problems this list holds -- what a reader would
// count, which is one per group and not one per subject.
func (l List) Count() int { return len(l.Merge()) }
