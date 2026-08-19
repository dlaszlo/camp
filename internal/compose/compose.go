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
// live directory's lock. An entry whose marker is missing or unreadable
// is reported and left alone, because camp removes only what it can prove
// is its own.
//
// The current run never sweeps its own entry: it holds that live lock
// itself.
func Sweep(root pathx.Root, isLive func(string) bool) (swept []string, kept []string) {
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
		if !isLive(live) {
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
