package plan

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/refusal"
)

// Gate is the overlap check, run at every up before anything is mounted.
//
// If a name exists in both the workspace root and the code root and
// allow_overlap does not name it, the composition does not start. This
// applies to directories as much as to files: a directory overlap is a
// merge, and a merge is a real decision about which repository owns what,
// not a detail to be discovered later.
//
// There is no --force. The escape hatch is allow_overlap in the
// configuration -- the same decision, recorded and diffable, rather than
// a flag typed once in a hurry and forgotten. Nothing can wall anybody
// in, because the repositories are ordinary directories that stay
// reachable without camp.
//
// In the steady state this fires almost never: the migration leaves zero
// overlap, allow_overlap holds one name, and the gate's real job is to
// notice the day something changes.
func Gate(cfg config.Config, lowerRoot, upperRoot []pathx.Info) refusal.List {
	var refused refusal.List

	covered := rootTargets(cfg)
	upperByName := map[string]pathx.Info{}
	for _, entry := range upperRoot {
		upperByName[entry.Name] = entry
	}

	lowerPath := cfg.LowerPath()
	upperPath := cfg.UpperPath()

	for _, lower := range lowerRoot {
		upper, both := upperByName[lower.Name]
		if !both {
			continue
		}

		// A name a mount target covers completely can never leak: the
		// overlay's merge at that path is never visible, because the mount
		// stands over it. Without this exemption the gate would be
		// unsatisfiable on .git, which both repositories have and neither
		// can give up.
		if covered[lower.Name] {
			continue
		}

		if cfg.AllowsOverlap(lower.Name) {
			refused.Extend(descend(cfg, lowerPath, upperPath,
				pathx.Rel{}.Append(lower.Name), lower, upper))
			continue
		}

		refused = append(refused, overlapRefusal(cfg, lowerPath, upperPath,
			pathx.Rel{}.Append(lower.Name), lower, upper))
	}
	return refused
}

// descend compares the two sides inside an allow-listed directory.
//
// A file present on both sides within a merged directory is exactly the
// trace of a copy-up: the same name, in a directory the two repositories
// share, where one copy now shadows the other. That is the thing most
// worth catching, and it is why allowing an overlap does not switch the
// check off, only move it one level down.
func descend(cfg config.Config, lowerPath, upperPath string, rel pathx.Rel, lower, upper pathx.Info) refusal.List {
	var refused refusal.List
	if lower.Type != pathx.Dir || upper.Type != pathx.Dir {
		return nil
	}

	lowerEntries, err := pathx.ReadDirBeneath(lowerPath, rel.Components())
	if err != nil {
		return nil
	}
	upperEntries, err := pathx.ReadDirBeneath(upperPath, rel.Components())
	if err != nil {
		return nil
	}

	upperByName := map[string]pathx.Info{}
	for _, entry := range upperEntries {
		upperByName[entry.Name] = entry
	}
	for _, entry := range lowerEntries {
		other, both := upperByName[entry.Name]
		if !both {
			continue
		}
		child := rel.Append(entry.Name)
		if entry.Type == pathx.Dir && other.Type == pathx.Dir {
			refused.Extend(descend(cfg, lowerPath, upperPath, child, entry, other))
			continue
		}
		refused = append(refused, overlapRefusal(cfg, lowerPath, upperPath, child, entry, other))
	}
	return refused
}

func overlapRefusal(cfg config.Config, lowerPath, upperPath string, rel pathx.Rel, lower, upper pathx.Info) refusal.R {
	path := rel.String()
	lowerFull := filepath.Join(lowerPath, path)
	upperFull := filepath.Join(upperPath, path)

	var consequence string
	switch {
	case lower.Type == pathx.Dir && upper.Type == pathx.Dir:
		consequence = "Both are directories, so the overlay would merge them: the " +
			"composed tree would show the two repositories' entries side by side " +
			"in one directory, and a new file created there would land in the " +
			"code repository whatever it looked like it belonged to. Which " +
			"repository owns that directory is a decision, and camp will not " +
			"make it silently."
	case upper.Type == pathx.File || upper.Type == pathx.Dir:
		consequence = fmt.Sprintf(
			"The side that matters is the code repository's: the upper layer wins "+
				"outright for a name that is not a directory on both sides, so the "+
				"composed tree would show %s and the workspace's copy would be "+
				"unreachable -- present on disk, invisible in the tree you are "+
				"working in.", upperFull)
	default:
		consequence = "The upper layer wins outright, so the workspace's copy " +
			"would be unreachable from the composed tree."
	}

	return refusal.New("overlap",
		"%q exists in both repositories and allow_overlap does not name it.\n"+
			"  workspace: %s (%s)\n"+
			"  code:      %s (%s)\n"+
			"%s\n"+
			"Two ways out, and nothing is mounted yet, so both are safe to do "+
			"right now:\n"+
			"  - move or rename one of the two, which is what the migration is "+
			"for; or\n"+
			"  - decide the overlap is intended and write it down, by adding "+
			"%q to allow_overlap in %s. Inside an allow-listed directory camp "+
			"keeps checking one level further down, because a file on both sides "+
			"of a merged directory is what a copy-up leaves behind.",
		path, lowerFull, lower.Type, upperFull, upper.Type, consequence,
		rel.First(), cfg.Source)
}

// GateSummary renders what the gate compared, for plan and doctor: how
// many names each side has at its root, and which ones are exempt and
// why. A check nobody can see the workings of is a check nobody trusts.
func GateSummary(cfg config.Config, lowerRoot, upperRoot []pathx.Info) string {
	covered := rootTargets(cfg)
	upperNames := map[string]bool{}
	for _, entry := range upperRoot {
		upperNames[entry.Name] = true
	}

	var exempt, allowed []string
	for _, entry := range lowerRoot {
		if !upperNames[entry.Name] {
			continue
		}
		switch {
		case covered[entry.Name]:
			exempt = append(exempt, entry.Name)
		case cfg.AllowsOverlap(entry.Name):
			allowed = append(allowed, entry.Name)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "the gate compares %d workspace root names against %d code "+
		"root names\n", len(lowerRoot), len(upperRoot))
	if len(exempt) > 0 {
		fmt.Fprintf(&b, "  exempt, covered by a mount target: %s\n", strings.Join(exempt, ", "))
	}
	if len(allowed) > 0 {
		fmt.Fprintf(&b, "  allowed by allow_overlap: %s\n", strings.Join(allowed, ", "))
	}
	return b.String()
}
