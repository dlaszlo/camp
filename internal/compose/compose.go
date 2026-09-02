// Package compose builds a composition and takes it apart.
//
// It is the middle of the tool: the plan says what, mountx says how, this
// says in what order and what happens when a step fails. The split it
// rests on is the point -- a frame that always executes, the
// configuration's steps in the middle of it, and verification before
// anything is declared up.
//
// One rule governs failure: up may refuse, and a refusal leaves nothing
// mounted. Every mount that was made is removed in reverse before the
// refusal is reported. If that unwinding itself cannot finish, the
// composition is *partly up*, and it is said so, with what remains listed
// by path -- never quietly detached to make the report look clean.
package compose

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/enc"
	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/holders"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/mountx"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/verify"
)

// MarkerName is the file every work and storage directory carries, so
// that anything left by a crash can be attributed to the composition it
// belonged to.
//
// It exists because the locks are held on inodes and there is no lock
// file to test: the sweeper reads the marker instead, and an entry is
// stale when the live directory it names is gone, or when a non-blocking
// lock on that live directory succeeds -- nobody is composing there.
const MarkerName = ".camp-target"

// Setup is one build.
type Setup struct {
	Plan plan.Plan
	// Prefix is where the tree is being built: the live path, or the
	// staging root in the privileged mode.
	Prefix string
	// Exclude is the validated payload, empty when nothing generates one.
	Exclude []byte
	UID     int
	GID     int
}

// Target returns where one mount goes in this build.
func (s Setup) Target(mount plan.Mount) string {
	if !mount.InLive {
		return mount.Target
	}
	if mount.Rel.Empty() {
		return s.Prefix
	}
	return mount.Rel.Join(s.Prefix)
}

// Directories creates everything camp provides for itself, before any
// mount.
//
// Nothing here is inside a repository, and nothing here can be. The paths
// come from fsx areas, and an area is a root camp holds open with the
// components below it: every one of them is resolved by the kernel,
// following no symlink and never leaving the root, in the call that
// creates the directory. A link planted at .camp/work does not redirect
// this; it stops it.
func Directories(p plan.Plan) error {
	work := fsx.Work(p.Config.Root, p.Hash)
	if _, err := work.MkdirAll(); err != nil {
		return err
	}
	if err := marker(work, p); err != nil {
		return err
	}

	// The overlay's work directory has to be empty. The kernel leaves its
	// own work/ inside it, mode 000 and owned by us, and a previous run's
	// leftover is exactly what would make this mount fail.
	if err := work.RemoveTree("work"); err != nil {
		return err
	}
	if _, err := work.MkdirAll("work"); err != nil {
		return err
	}

	if len(p.IslandsMounts) > 0 || hasStores(p) {
		storage := fsx.Storage(p.Config.Root, p.Hash)
		if _, err := storage.MkdirAll(); err != nil {
			return err
		}
		if err := marker(storage, p); err != nil {
			return err
		}
	}

	for _, mount := range p.Mounts {
		if mount.Role != plan.Store {
			continue
		}
		storage := fsx.Storage(p.Config.Root, p.Hash)
		if _, err := storage.MkdirDeep(mount.Rel.Components()); err != nil {
			return err
		}
	}
	return nil
}

func hasStores(p plan.Plan) bool {
	for _, mount := range p.Mounts {
		if mount.Role == plan.Store {
			return true
		}
	}
	return false
}

// marker writes the attribution file, if it is not already there.
func marker(area fsx.Area, p plan.Plan) error {
	path, err := area.Path(MarkerName)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	content := enc.Document([]string{
		enc.Line("live", p.Live),
		enc.Line("config", p.Config.Source),
	})
	return area.Write(MarkerName, content, 0o644)
}

// ReadMarker reads a work or storage directory's attribution, by name.
//
// For the callers asking about a directory camp holds no capability for:
// doctor walking what is on the machine, and the sweep. Where the answer
// decides what a privileged step then removes, the bytes come from a
// descriptor beneath a held root and go to ParseMarker instead -- a
// marker read through a name says nothing about the directory something
// else is about to act in.
func ReadMarker(directory string) (live, config string, err error) {
	path := filepath.Join(directory, MarkerName)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	return ParseMarker(path, data)
}

// ParseMarker reads an attribution out of a marker's own bytes.
//
// Separate from the reading so that a caller who obtained the bytes some
// other way -- through a descriptor it already holds, which is what the
// privileged helper does -- gets the same answer from the same code. The
// name is for the message only.
func ParseMarker(name string, data []byte) (live, config string, err error) {
	records, err := enc.Parse(data)
	if err != nil {
		return "", "", err
	}
	for _, record := range records {
		if len(record) != 2 {
			continue
		}
		switch record[0] {
		case "live":
			live = record[1]
		case "config":
			config = record[1]
		}
	}
	if live == "" {
		return "", "", fmt.Errorf("%s names no live directory", name)
	}
	return live, config, nil
}

// Result is what a build left behind.
type Result struct {
	// Mounted is what was made, in the order it was made, so that a
	// teardown can walk it backwards.
	Mounted []plan.Mount
	// Refused is why the build stopped, empty when it did not.
	Refused refusal.List
	// Stranded is what a failed rollback could not remove. Non-empty means
	// the composition is partly up.
	Stranded []string
}

// OK reports whether the composition is up.
func (r Result) OK() bool { return r.Refused.Empty() && len(r.Stranded) == 0 }

// Build performs the mount sequence and verifies it.
//
// On any failure the mounts made so far are removed in reverse and the
// reason is returned. Nothing is left mounted unless the removal itself
// failed, and then that is said.
func Build(s Setup) Result {
	result := Result{}

	for _, mount := range s.Plan.Mounts {
		operation := mount
		operation.Target = s.Target(mount)
		placed, err := mountx.Mount(operation)
		// Recorded as soon as the mount exists rather than when the whole
		// operation finishes. A read-only bind is two calls and the
		// propagation change a third, so a failure after the first leaves a
		// mount standing -- and unwinding a list that did not include it
		// would report a clean machine that is not clean.
		if placed {
			result.Mounted = append(result.Mounted, mount)
		}
		if err != nil {
			result.Stranded = unwind(s, result.Mounted)
			left := "Nothing is left mounted: the mounts made before this one " +
				"have been removed."
			if len(result.Stranded) > 0 {
				left = "What could not be removed is listed above."
			}
			result.Refused.Add("mount-failed", "%v\n  (%s)\n%s", err, mount.Why, left)
			return result
		}
	}

	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		result.Refused.Add("mount-table-unreadable", "%v", err)
		result.Stranded = unwind(s, result.Mounted)
		return result
	}

	result.Refused = verify.Run(verify.Input{
		Plan:      s.Plan,
		Prefix:    s.Prefix,
		LowerPath: s.Plan.Config.LowerPath(),
		Storage:   s.Plan.Storage,
		Table:     table,
		Exclude:   s.Exclude,
		UID:       s.UID,
		GID:       s.GID,
	})
	if !result.Refused.Empty() {
		result.Stranded = unwind(s, result.Mounted)
	}
	return result
}

// Check runs the verification pass against a composition that is already
// up, changing nothing.
//
// It is the same pass Build runs, and that is the point: status is this
// code path with the other exit -- reporting instead of refusing -- so
// there is no second definition of what "up" means to drift away from the
// first.
func Check(s Setup) refusal.List {
	var refused refusal.List
	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		refused.Add("mount-table-unreadable", "%v", err)
		return refused
	}
	return verify.Run(verify.Input{
		Plan:      s.Plan,
		Prefix:    s.Prefix,
		LowerPath: s.Plan.Config.LowerPath(),
		Storage:   s.Plan.Storage,
		Table:     table,
		Exclude:   s.Exclude,
		UID:       s.UID,
		GID:       s.GID,
	})
}

// unwind removes what was mounted, in reverse, and returns what it could
// not remove.
func unwind(s Setup, mounted []plan.Mount) []string {
	var stranded []string
	for index := len(mounted) - 1; index >= 0; index-- {
		target := s.Target(mounted[index])
		outcome, _ := mountx.Unmount(target)
		if outcome == mountx.Busy {
			stranded = append(stranded, target)
		}
	}
	return stranded
}

// Teardown is one attempt at taking a composition down.
type Teardown struct {
	// Removed is what came away.
	Removed []string
	// Absent is what was not mounted to begin with.
	Absent []string
	// Stuck is what could not be removed, with whoever is holding it.
	Stuck []Stuck
}

// Stuck is one mount that would not come down.
type Stuck struct {
	Target  string
	Reason  string
	Holders holders.Report
}

// Done reports whether everything came away.
func (t Teardown) Done() bool { return len(t.Stuck) == 0 }

// Down removes a list of targets in the order given, which is the reverse
// of the order they were made.
//
// It never refuses to try, and it never lies about the result. A mount
// something is holding stays mounted, is reported as still mounted, and
// makes the command exit non-zero. There is no lazy detach: that would
// take the mount out of the kernel's table while it is still alive and
// still being written through, and in the privileged mode the table is
// the only guard against a second composition on the same upper.
func Down(targets []string) Teardown {
	report := Teardown{}
	for _, target := range targets {
		outcome, err := mountx.Unmount(target)
		switch outcome {
		case mountx.Unmounted:
			report.Removed = append(report.Removed, target)
		case mountx.Absent:
			report.Absent = append(report.Absent, target)
		default:
			report.Stuck = append(report.Stuck, Stuck{
				Target:  target,
				Reason:  errorText(err),
				Holders: holders.Find(target),
			})
		}
	}
	return report
}

func errorText(err error) string {
	if err == nil {
		return "the kernel refused to remove it"
	}
	return err.Error()
}

// RemoveWorkDir removes the whole work directory for a composition that
// is down.
//
// Taken from the environment root and the hash rather than from a plan,
// because the one caller is the privileged teardown and it works from the
// record: the configuration may have been edited, or deleted, while the
// composition was up, and the record is what says where this composition
// put things. That caller opens the root itself, from the path the record
// carries.
func RemoveWorkDir(root pathx.Root, hash string) error {
	area := fsx.Work(root, hash)
	entries, err := os.ReadDir(area.Root())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if err := area.RemoveTree(entry.Name()); err != nil {
			return err
		}
	}
	return area.RemoveSelf()
}

// Residue reports what is left in the live directory after a teardown.
//
// Reported, never removed. If the composed tree's directory is not empty
// once everything is unmounted, that is evidence of a problem -- content
// that was written somewhere it should not have been -- and cleaning it
// away would destroy the only sign of it.
func Residue(live string) ([]string, error) {
	entries, err := os.ReadDir(live)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	enc.SortNames(names)
	return names, nil
}

// Sweep removes stale work directories left by sessions that are gone.
//
// The namespace mode has no down -- the kernel tears the namespace down,
// including the mounts, but the work directory is on the real filesystem
// and outlives it. So the next up sweeps: an entry is stale when the live
// directory its marker names no longer exists, or when nothing holds that
// live directory's lock -- ended answers that -- and, before either is
// believed, when no overlay in the table is standing on it. An entry
// whose marker is missing or unreadable is reported and left alone,
// because camp removes only what it can prove is its own; so is one whose
// use camp cannot rule out.
//
// The table is asked first because the lock can be wrong in one
// direction, and it is the direction that loses data. Measured, from
// inside a running session: /proc/locks shows no row for the launcher's
// lock, whose owner is outside the reader's pid namespace; and the live
// path is the overlay's own root, a different inode from the directory
// the launcher locked, so a non-blocking flock on it succeeds. Both
// together report the session camp is standing in as one that has ended.
// The workdir= option is the fact the lock cannot be: an overlay that
// names a work directory is using it.
//
// The caller holds the work lock (locks.Work) across this call, and a
// launcher creating a work directory holds the same lock from making the
// live directory to taking the live lock. Without it the decision and the
// removal are two moments: a launcher that had read the table and probed
// the lock, and was then descheduled, would remove what a second launcher
// created and mounted in between -- in a namespace whose mounts this
// table does not show. The lock keeps the decision true until the
// removal is done.
//
// One loss this does not close, recorded so it is not mistaken for
// covered: an environment directory renamed while a session runs. The
// marker records the live path from before the rename, which no longer
// exists, so ended reports the session over; and the running session's
// overlay is in its own mount namespace, invisible here (C20), so the
// table cannot say otherwise. From inside that session FromInside refuses
// before this runs, which is the reachable route -- an agent runs camp
// inside its own session. From a separate outside terminal it stays open,
// and the mount table is no help there. Closing it would need the marker
// to name something that survives the rename, which is a larger change
// than this defect.
//
// The current run never sweeps its own entry: it holds that live lock
// itself.
func Sweep(root pathx.Root, table []mountinfo.Entry, ended func(string) bool) (swept []string, kept []string) {
	work := filepath.Join(root.Name(), config.Dir, "work")
	entries, err := os.ReadDir(work)
	if err != nil {
		return nil, nil
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		directory := filepath.Join(work, entry.Name())
		live, _, err := ReadMarker(directory)
		if err != nil {
			kept = append(kept, fmt.Sprintf("%s (no readable %s: %v)",
				directory, MarkerName, err))
			continue
		}
		used, doubt := inUse(table, root, entry.Name())
		if used {
			continue
		}
		if doubt != "" {
			kept = append(kept, fmt.Sprintf("%s (%s)", directory, doubt))
			continue
		}
		if !ended(live) {
			continue
		}
		if err := fsx.Work(root, entry.Name()).RemoveTree("work"); err != nil {
			kept = append(kept, fmt.Sprintf("%s (%v)", directory, err))
			continue
		}
		if err := removeIfEmpty(fsx.Work(root, entry.Name()), directory); err != nil {
			kept = append(kept, fmt.Sprintf("%s (%v)", directory, err))
			continue
		}
		swept = append(swept, directory)
	}
	return swept, kept
}

// inUse reports whether an overlay in the table is standing on this
// entry's work directory -- and, when that cannot be told for certain,
// why not.
//
// By name first: the kernel shows the string it was given, and camp gives
// it the path it built the directory under. By inode second, resolved
// from the root of the filesystem with no symlink followed, the way every
// path camp acts on is resolved. What stays uncertain is kept: a spelling
// that is not a clean absolute path -- camp writes no other -- or one
// whose resolution meets a symlink, because the kernel followed it at
// mount time and camp cannot know to where. A work directory camp cannot
// look at is not camp's own: camp reads its own from the environment root
// down. Measured on this machine: eleven of twelve overlays belong to a
// container runtime, with work directories under a root-only tree, and
// keeping every camp entry over those would have every sweep here remove
// nothing and warn eleven times.
func inUse(table []mountinfo.Entry, root pathx.Root, hash string) (used bool, doubt string) {
	parts := []string{config.Dir, "work", hash, "work"}
	own := filepath.Join(append([]string{root.Name()}, parts...)...)
	mine, err := root.Stat(parts)
	if err != nil {
		return false, fmt.Sprintf("its work directory could not be looked at: %v", err)
	}
	for _, entry := range mountinfo.AllOverlays(table) {
		path := mountinfo.WorkOf(entry)
		switch {
		case path == "":
			continue
		case path == own:
			return true, ""
		case mountinfo.SpelledAmbiguously(path):
			// A backslash the kernel's octal escaping never writes: a foreign
			// overlay mounted through the legacy option string, whose real
			// spelling camp cannot recover (mountinfo.SpelledAmbiguously). It
			// could be this directory under a name camp cannot read, so it is
			// kept rather than swept out from under whatever holds it.
			return false, fmt.Sprintf("the overlay at %s names its work directory %q, "+
				"whose spelling camp cannot decode with certainty, so it cannot "+
				"compare it with this one", entry.Point, path)
		case !filepath.IsAbs(path) || path != filepath.Clean(path):
			return false, fmt.Sprintf("the overlay at %s names its work directory %q, "+
				"which camp cannot compare with this one for certain", entry.Point, path)
		}
		theirs, err := pathx.StatBeneath("/", strings.Split(path[1:], "/"))
		switch {
		case errors.Is(err, os.ErrPermission):
			// A work directory under a tree camp may not read is not camp's:
			// camp resolves its own from the environment root down, which it
			// can always read. Treating unreadable as in use would keep every
			// entry over the container runtime's overlays -- eleven of twelve
			// here, measured -- and sweep nothing, ever. The one way to reach
			// this with a directory that is camp's is to bind one's own work
			// directory elsewhere and strip search permission from its parent,
			// which is a person hiding their own running session from the tool
			// meant to protect it -- outside the threat model.
			continue
		case err != nil:
			return false, fmt.Sprintf("the overlay at %s names its work directory %s, "+
				"and camp cannot tell whether that is this one: %v", entry.Point, path, err)
		case theirs.Type == pathx.Symlink:
			return false, fmt.Sprintf("the overlay at %s names its work directory %s, "+
				"which is a symbolic link, and camp cannot tell where the kernel "+
				"followed it to", entry.Point, path)
		case mine.Exists() && theirs.Exists() && theirs.Ident == mine.Ident:
			return true, ""
		}
	}
	return false, ""
}

func removeIfEmpty(area fsx.Area, directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := area.RemoveTree(entry.Name()); err != nil {
			return err
		}
	}
	return area.RemoveSelf()
}

// DescribeStuck renders one mount that would not come down, with what to
// do about it.
func DescribeStuck(stuck Stuck) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is still mounted: %s\n", stuck.Target, stuck.Reason)
	if !stuck.Holders.Any() {
		b.WriteString("  camp could not find what is holding it. ")
		if caveat := stuck.Holders.Caveat(); caveat != "" {
			b.WriteString(caveat)
		}
		b.WriteString("\n")
		return b.String()
	}
	for _, holder := range stuck.Holders.Holders {
		fmt.Fprintf(&b, "  held by %s\n", holder.Describe())
	}
	b.WriteString("  A composition cannot be unmounted from under a process " +
		"standing in it. Leave that directory or close that file, then run " +
		"'camp down' again.\n")
	if caveat := stuck.Holders.Caveat(); caveat != "" {
		fmt.Fprintf(&b, "  %s\n", caveat)
	}
	return b.String()
}
