package gen

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/dlaszlo/camp/internal/islands"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
)

// Validate treats the generation step's output as hostile data.
//
// It has to be. Whoever can edit the configuration can choose the
// program that runs at prepare, and the mounts that follow are made by a
// process holding the mount capability. A generator that could name any
// entry it liked would be steering those mounts, so nothing it produced
// steers one unchecked.
//
// Static validation before generation checks everything configured; this
// pass checks everything generated; nothing is mounted on the strength of
// either alone.
func Validate(built plan.Plan, out Output, expected []byte) refusal.List {
	var refused refusal.List

	if !bytes.Equal(out.Exclude, expected) {
		refused.Add("generate-exclude-mismatch",
			"the generation step's exclude payload is not what camp's own "+
				"assembly of it is: %d bytes were produced and %d were expected.\n"+
				"The payload has to be the code repository's own exclude bytes, "+
				"unchanged and complete, followed by camp's block. Comparing only "+
				"the marker line would accept a payload whose repository half had "+
				"been dropped -- and then git inside the composed tree would "+
				"quietly stop honouring the repository's own patterns.",
			len(out.Exclude), len(expected))
	}

	for _, mount := range built.IslandsMounts {
		refused.Extend(validateIslands(built, mount, out.Islands[mount.Target.String()]))
	}
	return refused
}

func validateIslands(built plan.Plan, mount plan.Islands, entries []islands.Entry) refusal.List {
	var refused refusal.List
	seen := map[string]bool{}

	for _, entry := range entries {
		switch {
		case entry.Name == "":
			refused.Add("generate-islands-entry",
				"the islands list for %q has an entry with an empty name.",
				mount.Target.String())
			continue
		case strings.Contains(entry.Name, "/"):
			refused.Add("generate-islands-entry",
				"the islands list for %q has the entry %q, which is a path.\n"+
					"An island is one entry of the source directory: exactly one "+
					"path component. Anything else would let the generator reach "+
					"outside the directory it was asked about.",
				mount.Target.String(), entry.Name)
			continue
		case entry.Name == "." || entry.Name == "..":
			refused.Add("generate-islands-entry",
				"the islands list for %q has the entry %q, which names a "+
					"directory other than an entry of the source.",
				mount.Target.String(), entry.Name)
			continue
		case seen[entry.Name]:
			refused.Add("generate-islands-duplicate",
				"the islands list for %q names %q twice. Two mounts on one path "+
					"is not a thing camp will guess about.",
				mount.Target.String(), entry.Name)
			continue
		}
		seen[entry.Name] = true

		if entry.Type != pathx.Dir && entry.Type != pathx.File {
			refused.Add("generate-islands-type",
				"the islands list for %q declares %q as a %s. camp stands a "+
					"directory or a regular file in the water and nothing else.",
				mount.Target.String(), entry.Name, entry.Type)
			continue
		}

		// Resolved from the environment root, one component at a time,
		// following no symlink and never leaving it: what the generator
		// named has to be an entry of the directory it was asked about, and
		// not something reached through one.
		parts := append(append([]string{}, mount.SourceParts...), entry.Name)
		info, err := pathx.StatBeneath(built.Config.Env, parts)
		switch {
		case err != nil:
			refused.Add("generate-islands-unreachable",
				"the islands list for %q names %q, which camp could not look at "+
					"inside %s: %v.", mount.Target.String(), entry.Name, mount.Source, err)
		case !info.Exists():
			refused.Add("generate-islands-absent",
				"the islands list for %q names %q, and %s has no such entry.\n"+
					"An island stands for something the source really contributes. "+
					"camp will not mount a name the source does not have.",
				mount.Target.String(), entry.Name, mount.Source)
		case info.Type != entry.Type:
			refused.Add("generate-islands-type-mismatch",
				"the islands list for %q declares %q as a %s, and it is a %s in "+
					"%s.\nThe type decides whether camp binds a directory or a file, "+
					"and the two do not substitute for each other.",
				mount.Target.String(), entry.Name, entry.Type, info.Type, mount.Source)
		}
	}
	return refused
}

// ValidateExpanded re-runs the order and tracked-content rules over the
// concrete mounts, now that the islands are known.
//
// The static pass could only walk what the configuration named. These
// mounts did not exist then, and they are subject to exactly the same
// rules: no two mounts on one target, no earlier target inside a later
// one, and nothing covering content the code repository tracks.
func ValidateExpanded(expanded plan.Plan, tracks func(string) []string) refusal.List {
	var refused refusal.List

	var placed []plan.Mount
	for _, mount := range expanded.Mounts {
		if !mount.InLive || mount.Rel.Empty() {
			continue
		}
		for _, earlier := range placed {
			switch {
			case earlier.Rel.Equal(mount.Rel):
				refused.Add("target-duplicate",
					"two mounts end up on the target %q once the islands are "+
						"expanded. The second would cover the first completely.",
					mount.Rel.String())
			case earlier.Rel.Inside(mount.Rel):
				refused.Add("target-nested",
					"once the islands are expanded, the mount at %q comes before "+
						"the mount at %q, which contains it -- the second would "+
						"silently cover the first.",
					earlier.Rel.String(), mount.Rel.String())
			}
		}
		placed = append(placed, mount)

		if tracks == nil {
			continue
		}
		if tracked := tracks(mount.Rel.String()); len(tracked) > 0 {
			shown := tracked
			if len(shown) > 5 {
				shown = append(shown[:5:5], fmt.Sprintf("and %d more", len(tracked)-5))
			}
			refused.Add("target-tracked-code",
				"the expanded mount at %q would cover content the code "+
					"repository tracks: %s.",
				mount.Rel.String(), strings.Join(shown, ", "))
		}
	}
	return refused
}
