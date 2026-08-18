// Package drift is the read-only pass camp runs when a session ends.
//
// It runs at `down`, and at the end of a namespace session, and it never
// blocks anything -- a teardown that refused would wall the user in. What
// it does is tell, while the cause is still fresh. The end of a session
// is the moment somebody still remembers what they did.
//
// Four scans, and the fourth is the one that matters most.
//
// The gate's comparison is re-run, so an overlap that appeared during the
// session is named the same day rather than at the next up. The inventory
// comparison is re-run for the same reason. The code repository's
// untracked paths whose first component is a workspace root name or a
// mount target are reported as suspected copy-up residue.
//
// And then the index is scanned. `git add -f` reads a workspace file
// through the composed tree and stages its bytes -- the read-only mounts
// stop writes, not reads, and nothing prevents this. What it leaves is an
// *indexed* path with no file in the raw working tree at all, which means
// a scan for untracked files is structurally blind to exactly the leak
// this pass exists to find. `git ls-files --stage` sees it.
//
// The framing that makes detection worth having: the point of no return
// for a shared history is push, not commit. A leak caught at down is
// usually still free -- git reset in the code repository, composition
// down, the user's own hand -- and the last automated gate before that
// point, the repository's own pre-push hook, measurably runs in both
// modes.
package drift

import (
	"fmt"
	"strings"

	"github.com/dlaszlo/camp/internal/gitwire"
	"github.com/dlaszlo/camp/internal/inventory"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
)

// Worktree is one git worktree registered inside the composed tree, with
// the command that makes it survive.
type Worktree struct {
	Path string
	// Backing is where the checkout really lives, outside the composition.
	Backing string
	Branch  string
	// Repair is the exact command, ready to paste.
	Repair string
}

// Report is everything the pass found.
type Report struct {
	Worktrees []Worktree
	Overlaps  []string
	Inventory string
	Untracked []string
	Indexed   []string
	// Failures are scans that could not run, said out loud rather than
	// counted as "nothing found".
	Failures []string
}

// Empty reports whether there is nothing to say.
func (r Report) Empty() bool {
	return len(r.Worktrees) == 0 && len(r.Overlaps) == 0 && r.Inventory == "" &&
		len(r.Untracked) == 0 && len(r.Indexed) == 0 && len(r.Failures) == 0
}

// Scan runs all four, changing nothing.
func Scan(built plan.Plan) Report {
	report := Report{}
	if built.Live == "" {
		return report
	}

	// A scan that could not run is said, never left out: an omitted scan
	// reads exactly like a scan that found nothing, and these run at the
	// moment the cause is still fresh.
	code, state, err := gitwire.Open(built.Config.UpperPath())
	switch state {
	case gitwire.InWorkTree:
		report.worktrees(built, code)
		report.leaks(built, code)
	case gitwire.Unreadable:
		report.Failures = append(report.Failures, fmt.Sprintf(
			"the worktree and leak scans did not run: git could not say whether "+
				"%s is a working tree (%v)", built.Config.UpperPath(), err))
	}

	report.gate(built)
	report.inventory(built)
	return report
}

// worktrees finds the checkouts whose registration dies with the
// composition.
//
// Git stores a worktree's git directory as an absolute path and compares
// it as a string, so a worktree created through the composed tree records
// the live path on both sides: after down, neither pointer resolves. The
// files are intact and git simply cannot see them. Worse, git prunes a
// dead registration after gc.worktreePruneExpire -- three months by
// default -- and auto-gc runs from ordinary commands, so this is the
// failure that happens while nobody is looking. Committed work survives
// on its branch; uncommitted work is stranded as plain files.
//
// `git worktree repair <path outside the composition>` rewrites both
// pointers to paths that outlive the composition, after which the
// worktree is composition-independent and stops dying at every down.
func (r *Report) worktrees(built plan.Plan, code *gitwire.Repo) {
	registered, err := code.Worktrees()
	if err != nil {
		r.Failures = append(r.Failures,
			fmt.Sprintf("the worktree list could not be read: %v", err))
		return
	}
	for _, worktree := range gitwire.WorktreesUnder(registered, built.Live) {
		backing, found := Backing(built, worktree.Path)
		entry := Worktree{Path: worktree.Path, Branch: worktree.Branch, Backing: backing}
		if found {
			entry.Repair = fmt.Sprintf("git -C %s worktree repair %s",
				built.Config.UpperPath(), backing)
		}
		r.Worktrees = append(r.Worktrees, entry)
	}
}

// Backing maps a path inside the composed tree to where its content
// really lives, so that a repair command can name a path that outlives
// the composition.
//
// The deepest mount whose target contains the path decides, because that
// is the one actually providing the content there.
func Backing(built plan.Plan, path string) (string, bool) {
	best := ""
	depth := -1
	for _, mount := range built.Mounts {
		if !mount.InLive || mount.Source == "" {
			continue
		}
		target := mount.Target
		if path != target && !strings.HasPrefix(path, target+"/") {
			continue
		}
		if length := len(target); length > depth {
			depth = length
			best = mount.Source + strings.TrimPrefix(path, target)
		}
	}
	if depth < 0 {
		// Not under any mount: the overlay itself provides it, so the
		// content is in the code repository.
		if strings.HasPrefix(path, built.Live+"/") {
			return built.Config.UpperPath() + strings.TrimPrefix(path, built.Live), true
		}
		return "", false
	}
	return best, true
}

// leaks runs the two scans over the code repository: what is untracked
// and looks like copy-up residue, and what is indexed and should not be.
func (r *Report) leaks(built plan.Plan, code *gitwire.Repo) {
	suspect := suspectNames(built)

	untracked, err := code.Untracked()
	if err != nil {
		r.Failures = append(r.Failures,
			fmt.Sprintf("the untracked-file scan could not run: %v", err))
	}
	for _, path := range untracked {
		if suspect[first(path)] {
			r.Untracked = append(r.Untracked, path)
		}
	}

	indexed, err := code.Indexed()
	if err != nil {
		r.Failures = append(r.Failures,
			fmt.Sprintf("the index scan could not run: %v", err))
		return
	}
	for _, path := range indexed {
		if suspect[first(path)] {
			r.Indexed = append(r.Indexed, path)
		}
	}
}

// suspectNames is every root name that belongs to the workspace or to a
// mount, minus the ones the code repository legitimately has.
func suspectNames(built plan.Plan) map[string]bool {
	suspect := map[string]bool{}
	for _, entry := range built.LowerRoot {
		suspect[entry.Name] = true
	}
	for _, mount := range built.Mounts {
		if mount.InLive && !mount.Rel.Empty() {
			suspect[mount.Rel.First()] = true
		}
	}
	// A name the code repository has at its own root is a code path, and
	// finding it in the code repository's index proves nothing.
	for _, entry := range built.UpperRoot {
		delete(suspect, entry.Name)
	}
	return suspect
}

func first(path string) string {
	name, _, _ := strings.Cut(path, "/")
	return name
}

func (r *Report) gate(built plan.Plan) {
	for _, refused := range plan.Gate(built.Config, built.LowerRoot, built.UpperRoot) {
		r.Overlaps = append(r.Overlaps, refused.Message)
	}
}

func (r *Report) inventory(built plan.Plan) {
	current := inventory.Take(built.LowerRoot, built.UpperRoot)
	r.Inventory = inventory.Report(built.Config.Env, current)
}

// Refresh re-reads both roots and runs the whole pass. Used at the end of
// a session, when the listings camp started with may be stale -- a name
// born during the session is exactly what this is looking for.
func Refresh(built plan.Plan) Report {
	lower, err := pathx.ReadDirBeneath(built.Config.LowerPath(), nil)
	if err == nil {
		built.LowerRoot = lower
	}
	upper, err := pathx.ReadDirBeneath(built.Config.UpperPath(), nil)
	if err == nil {
		built.UpperRoot = upper
	}
	return Scan(built)
}

// String renders the whole report for a person.
func (r Report) String() string {
	if r.Empty() {
		return ""
	}
	var b strings.Builder

	if len(r.Worktrees) > 0 {
		b.WriteString("git worktrees registered inside the composed tree:\n")
		for _, worktree := range r.Worktrees {
			fmt.Fprintf(&b, "  %s", worktree.Path)
			if worktree.Branch != "" {
				fmt.Fprintf(&b, "  (branch %s)", worktree.Branch)
			}
			b.WriteString("\n")
			if worktree.Repair != "" {
				fmt.Fprintf(&b, "      %s\n", worktree.Repair)
			}
		}
		b.WriteString("  Git records a worktree's git directory as an absolute " +
			"path, so one made\n" +
			"  through the composed tree points at the live path on both sides " +
			"and stops\n" +
			"  resolving the moment the composition comes down. The files stay " +
			"where they\n" +
			"  are; git simply cannot see them. Run the repair above and the " +
			"worktree\n" +
			"  becomes composition-independent -- otherwise git prunes the dead\n" +
			"  registration after gc.worktreePruneExpire, three months by " +
			"default, from an\n" +
			"  ordinary auto-gc. Committed work survives on its branch; " +
			"uncommitted work\n  is stranded as plain files.\n\n")
	}

	if len(r.Indexed) > 0 {
		b.WriteString("paths in the code repository's index that belong to the " +
			"workspace or to a mount:\n")
		for _, path := range r.Indexed {
			fmt.Fprintf(&b, "  %s\n", path)
		}
		b.WriteString("  This is what a forced add leaves behind. Nothing " +
			"prevents 'git add -f':\n" +
			"  the read-only mounts stop writes, and it only reads. The point of " +
			"no return\n" +
			"  is push, not commit, so this is very likely still free to undo:\n" +
			"      git -C <code repository> restore --staged <path>\n\n")
	}

	if len(r.Untracked) > 0 {
		b.WriteString("untracked paths in the code repository whose names " +
			"belong to the workspace:\n")
		for _, path := range r.Untracked {
			fmt.Fprintf(&b, "  %s\n", path)
		}
		b.WriteString("  Suspected copy-up residue: a write that should have " +
			"been refused and\n  landed in the code repository instead. Look at " +
			"them before committing.\n\n")
	}

	if len(r.Overlaps) > 0 {
		b.WriteString("the two repositories now overlap:\n")
		for _, message := range r.Overlaps {
			fmt.Fprintf(&b, "  %s\n\n", strings.ReplaceAll(message, "\n", "\n  "))
		}
	}

	if r.Inventory != "" {
		b.WriteString(r.Inventory)
		b.WriteString("\n")
	}

	if len(r.Failures) > 0 {
		b.WriteString("scans that could not run (so their silence means nothing):\n")
		for _, failure := range r.Failures {
			fmt.Fprintf(&b, "  %s\n", failure)
		}
		b.WriteString("\n")
	}
	return b.String()
}
