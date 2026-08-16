package plan

import (
	"github.com/dlaszlo/camp/internal/pathx"
)

// virtual is the composed tree as it will look at a given point in the
// mount sequence, without anything being mounted.
//
// It exists so that every target can be judged in the state its own step
// will really meet. A mount point supplied by an earlier bind counts as
// present; a path under a store that does not exist yet counts as absent.
// Answering those questions on paper is what keeps the one safe moment --
// nothing mounted, everything repairable by hand -- worth having.
type virtual struct {
	lower  string
	upper  string
	covers []cover
}

type cover struct {
	rel    pathx.Rel
	source string
	typ    pathx.Type
}

// cover records that a mount has been placed.
func (v *virtual) cover(rel pathx.Rel, source string, typ pathx.Type) {
	v.covers = append(v.covers, cover{rel: rel, source: source, typ: typ})
}

// at reports what stands at a path in the tree.
//
// The deepest cover wins, and among covers at the same path the latest
// does -- which is what the kernel would do, though the sequence check
// refuses that case before it can arise.
func (v *virtual) at(rel pathx.Rel) (pathx.Type, error) {
	best := -1
	for index, c := range v.covers {
		if !rel.Equal(c.rel) && !rel.Inside(c.rel) {
			continue
		}
		if best == -1 || len(c.rel.Components()) >= len(v.covers[best].rel.Components()) {
			best = index
		}
	}

	if best >= 0 {
		c := v.covers[best]
		if rel.Equal(c.rel) {
			return c.typ, nil
		}
		remainder := rel.Components()[len(c.rel.Components()):]
		info, err := pathx.StatBeneath(c.source, remainder)
		if err != nil {
			return pathx.Absent, err
		}
		return info.Type, nil
	}

	return v.overlay(rel)
}

// overlay answers what the merged root shows at a path, with nothing
// mounted over it.
//
// Directories merge and files do not: whatever the upper has at a path
// wins outright, and the lower is only consulted where the upper has
// nothing. That is the kernel's rule, and reproducing it here is what
// makes the paper walk agree with the real one.
func (v *virtual) overlay(rel pathx.Rel) (pathx.Type, error) {
	parts := rel.Components()
	upper, err := pathx.StatBeneath(v.upper, parts)
	if err != nil {
		return pathx.Absent, err
	}
	if upper.Exists() {
		return upper.Type, nil
	}
	lower, err := pathx.StatBeneath(v.lower, parts)
	if err != nil {
		return pathx.Absent, err
	}
	return lower.Type, nil
}
