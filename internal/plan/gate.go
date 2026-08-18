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
	refused, _ := compare(cfg, lowerRoot, upperRoot)
	return refused
}

// LowerOnlyInsideAllowed lists what only the workspace contributes inside
// an allow-listed directory, as paths relative to the merged root.
//
// This is the one place the specification asks for file-level
// enumeration, and the reason is the shape of an allow-listed directory:
// it is the only place where the two repositories' content genuinely
// mixes in one directory, so the coarse root line that covers every other
// workspace name would cover the code repository's own files there too.
// Without these lines the workspace's files inside such a directory are
// in no exclude at all -- 'git status' in the composed tree lists them as
// untracked and 'git add .' stages the workspace's content into the code
// repository, which is exactly what the exclude exists to stop.
//
// A directory only the workspace has yields one line for the whole
// subtree, so a file born in it mid-session is covered without camp
// re-reading anything. Only where both sides have a directory does the
// walk go deeper, because only there can the mixing continue.
func LowerOnlyInsideAllowed(cfg config.Config, lowerRoot, upperRoot []pathx.Info) []string {
	_, only := compare(cfg, lowerRoot, upperRoot)
	return only
}

// compare is the one walk both of those read: the gate's refusals and the
// exclude's lines come out of the same traversal of the same two trees,
// so they cannot start describing different sets.
func compare(cfg config.Config, lowerRoot, upperRoot []pathx.Info) (refusal.List, []string) {
	var refused refusal.List
	var lowerOnly []string

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
			problems, only := descend(cfg, lowerPath, upperPath,
				pathx.Rel{}.Append(lower.Name), lower, upper)
			refused.Extend(problems)
			lowerOnly = append(lowerOnly, only...)
			continue
		}

		refused = append(refused, overlapRefusal(cfg, lowerPath, upperPath,
			pathx.Rel{}.Append(lower.Name), lower, upper))
	}
	return refused, lowerOnly
}

// descend compares the two sides inside an allow-listed directory.
//
// A file present on both sides within a merged directory is exactly the
// trace of a copy-up: the same name, in a directory the two repositories
// share, where one copy now shadows the other. That is the thing most
// worth catching, and it is why allowing an overlap does not switch the
// check off, only move it one level down.
func descend(cfg config.Config, lowerPath, upperPath string, rel pathx.Rel, lower, upper pathx.Info) (refusal.List, []string) {
	var refused refusal.List
	var lowerOnly []string
	if lower.Type != pathx.Dir || upper.Type != pathx.Dir {
		return nil, nil
	}

	lowerEntries, err := pathx.ReadDirBeneath(lowerPath, rel.Components())
	if err != nil {
		return nil, nil
	}
	upperEntries, err := pathx.ReadDirBeneath(upperPath, rel.Components())
	if err != nil {
		return nil, nil
	}

	upperByName := map[string]pathx.Info{}
	for _, entry := range upperEntries {
		upperByName[entry.Name] = entry
	}
	for _, entry := range lowerEntries {
		child := rel.Append(entry.Name)
		other, both := upperByName[entry.Name]
		if !both {
			// Only the workspace has it, so it is the workspace's content
			// standing in a directory the two share. One line covers it,
			// whatever it is: a file, or a whole subtree with everything born
			// in it later.
			lowerOnly = append(lowerOnly, child.String())
			continue
		}
		if entry.Type == pathx.Dir && other.Type == pathx.Dir {
			problems, only := descend(cfg, lowerPath, upperPath, child, entry, other)
			refused.Extend(problems)
			lowerOnly = append(lowerOnly, only...)
			continue
		}
		refused = append(refused, overlapRefusal(cfg, lowerPath, upperPath, child, entry, other))
	}
	return refused, lowerOnly
}

// The overlap gate's two refusals, in the shape a rule that fires many
// times has to have: what happens at one name is on that name's own line,
// and the explanation is written once for however many there are.
//
// A composition that has drifted has drifted at several names at once --
// that is what a migration half-done looks like -- and the reader's next
// move is the same for all of them. Nine copies of these paragraphs would
// bury the nine paths that are the only thing to act on.
func overlapGroup(cfg config.Config) refusal.Group {
	return refusal.Group{
		Rule: "overlap",
		One:  "a name exists in both repositories and allow_overlap does not name it:",
		Many: "%d names exist in both repositories and allow_overlap names none " +
			"of them:",
		Detail: "A merge is a decision about which repository owns a directory: a " +
			"file created in a merged directory lands in the code repository, " +
			"whatever it looked like it belonged to. A name that is not a " +
			"directory on both sides is not merged at all -- the upper layer wins " +
			"outright, and the workspace's copy stays on disk and is invisible in " +
			"the tree you are working in. camp makes neither choice silently.\n" +
			"Two ways out, and nothing is mounted yet, so both are safe to do " +
			"right now:\n" +
			"  - move or rename one side of each pair, which is what the migration " +
			"is for; or\n" +
			"  - decide an overlap is intended and write it down, by adding the " +
			"name to allow_overlap in " + cfg.Source + ". Inside an allow-listed " +
			"directory camp keeps checking one level further down, because a file " +
			"on both sides of a merged directory is what a copy-up leaves behind.",
	}
}

// Inside an allow-listed directory there is no knob to point at.
// allow_overlap names root entries, and the check deliberately moves one
// level down rather than switching off -- so the advice that fits the
// root case sends the reader to a setting that is already there and would
// change nothing. Measured on a real composition: after a copy-up through
// an allow-listed directory, the refusal named the file and then advised
// adding the directory, which was allow-listed already.
func copyUpGroup() refusal.Group {
	return refusal.Group{
		Rule: "overlap",
		One: "a file exists on both sides of an allow-listed directory, which " +
			"is what a copy-up leaves behind:",
		Many: "%d files exist on both sides of an allow-listed directory, which " +
			"is what a copy-up leaves behind:",
		Detail: "A write through the composed tree copied the workspace's file " +
			"into the code repository, and both stand there now. allow_overlap " +
			"cannot name these -- it names root entries, and inside an " +
			"allow-listed directory the check moves one level down rather than " +
			"switching off, because this trace is the thing it exists to catch.\n" +
			"Decide which copy is the real one, and remove or rename the other. " +
			"Nothing is mounted, so both are safe to do right now.",
	}
}

func overlapRefusal(cfg config.Config, lowerPath, upperPath string, rel pathx.Rel, lower, upper pathx.Info) refusal.R {
	path := rel.String()
	lowerFull := filepath.Join(lowerPath, path)
	upperFull := filepath.Join(upperPath, path)

	consequence := "the code repository's copy wins and the workspace's is " +
		"unreachable"
	if lower.Type == pathx.Dir && upper.Type == pathx.Dir {
		consequence = "the overlay would merge the two"
	}
	subject := fmt.Sprintf("%q: workspace %s (%s) and code %s (%s) -- %s",
		path, lowerFull, lower.Type, upperFull, upper.Type, consequence)

	if len(rel.Components()) > 1 {
		return refusal.Of(copyUpGroup(), "%s, inside the allow-listed directory %q",
			subject, rel.First())
	}
	return refusal.Of(overlapGroup(cfg), "%s", subject)
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
