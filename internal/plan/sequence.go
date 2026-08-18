package plan

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/islands"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/refusal"
)

// Walking the mount sequence on paper.
//
// Every check here judges a target in the state its own step will really
// meet: a mount point an earlier bind supplies counts as present, and a
// later mount that would silently cover an earlier one is refused before
// either of them exists. That is only possible because steps: is a real
// total order, and it is the whole reason the language has one.

// checkSequence walks the mount sequence on paper, in its own order.
func (c *checker) checkSequence(built Plan) {
	tree := &virtual{lower: c.lower, upper: c.upper}
	var placed []Mount

	for _, mount := range built.Mounts {
		if !mount.InLive {
			continue
		}
		if mount.Role == Composed {
			// The composed root is the live directory, already checked.
			continue
		}

		c.checkOrder(mount, placed)
		sourceType := c.checkSource(mount)
		c.checkTarget(mount, sourceType, tree)
		c.checkTracked(mount)

		tree.cover(mount.Rel, coverSource(mount), sourceType)
		placed = append(placed, mount)
	}
}

// coverSource says what a mount makes visible at its target. For camp's
// own stores that is the storage directory, which may not exist yet --
// in which case nothing resolves under it, which is the truth.
func coverSource(mount Mount) string { return mount.Source }

// The rules a sequence can break at more than one place. Each says
// itself once and lists every pair that broke it: a steps: list somebody
// has reordered breaks at several places at once, and the reader's move is
// the same at all of them.
var (
	duplicateTargets = refusal.Group{
		Rule: "target-duplicate",
		One:  "two mounts declare the same target:",
		Many: "%d targets are declared by two mounts each:",
		Detail: "The second would cover the first completely, so the first would " +
			"exist in the mount table and be reachable by nothing. camp will not " +
			"mount something it knows is unreachable. Keep the one you meant.",
	}
	nestedTargets = refusal.Group{
		Rule: "target-nested",
		One:  "a mount is declared before a mount that contains it:",
		Many: "%d mounts are declared before a mount that contains them:",
		Detail: "Mounted in that order the second would silently cover the first: " +
			"it would still be listed in the kernel's mount table, and no path " +
			"would reach it. Parent first, then child -- swap the two.\n" +
			"The order in steps: is the mount order, which is exactly why it can " +
			"be checked before anything is mounted.",
	}
)

func (c *checker) checkOrder(mount Mount, placed []Mount) {
	for _, earlier := range placed {
		switch {
		case earlier.Rel.Equal(mount.Rel):
			c.refused.Group(duplicateTargets, "%q: %s, and then %s",
				mount.Rel.String(), describeOrigin(earlier), describeOrigin(mount))
		case earlier.Rel.Inside(mount.Rel):
			c.refused.Group(nestedTargets, "%q (%s) is declared before %q (%s), "+
				"which contains it",
				earlier.Rel.String(), describeOrigin(earlier),
				mount.Rel.String(), describeOrigin(mount))
		}
	}
}

func describeOrigin(mount Mount) string {
	if mount.Step < 0 {
		return fmt.Sprintf("derived, %s", mount.Role)
	}
	return fmt.Sprintf("step %d", mount.Step+1)
}

// What a mount's two ends can be wrong about. A composition whose
// repository is not checked out, or whose steps: were written against a
// tree that has since moved, fails one of these at every mount it
// declares -- so each names every mount it fired for and explains itself
// once.
var (
	unreadableSources = refusal.Group{
		Rule: "source-unreadable",
		One:  "a mount source could not be looked at:",
		Many: "%d mount sources could not be looked at:",
		Detail: "Every component of a source path is opened without following a " +
			"symlink, so a permission or a type on the way down stops the look " +
			"as surely as an absent directory does.",
	}
	missingSources = refusal.Group{
		Rule: "source-missing",
		One:  "a mount source does not exist:",
		Many: "%d mount sources do not exist:",
		Detail: "camp creates nothing inside a repository. Either the source is " +
			"wrong, or the directory has to be added to the repository that owns " +
			"it and committed -- git cannot track an empty directory, so a mount " +
			"point needs a placeholder file in it.",
	}
	symlinkSources = refusal.Group{
		Rule: "source-symlink",
		One:  "a mount source is a symbolic link:",
		Many: "%d mount sources are symbolic links:",
		Detail: "A bind mount follows symlinks, so it would attach whatever the " +
			"link points at to the composed tree -- possibly a directory nowhere " +
			"near the repository it was supposed to come from. Point the source " +
			"at the real path.",
	}
	sourceTypes = refusal.Group{
		Rule: "source-type",
		One:  "a mount source is something camp cannot bind:",
		Many: "%d mount sources are things camp cannot bind:",
		Detail: "camp binds directories and regular files, nothing else. A " +
			"socket, a FIFO or a device has no honest reading as a mount source.",
	}
	unreadableTargets = refusal.Group{
		Rule: "target-unreadable",
		One:  "a mount point could not be looked at:",
		Many: "%d mount points could not be looked at:",
		Detail: "The sequence is walked on paper, over the tree as each step will " +
			"really meet it, and this step's mount point could not be resolved " +
			"in it.",
	}
	missingTargets = refusal.Group{
		Rule: "target-missing",
		One: "a mount point does not exist in the composed tree, so the mount " +
			"declared on it has nothing to attach to:",
		Many: "%d mount points do not exist in the composed tree, so the mounts " +
			"declared on them have nothing to attach to:",
		Detail: "A bind mount cannot create its own mount point, and by the time " +
			"these mounts happen the tree underneath them is read-only, so camp " +
			"cannot create one either. Commit an empty directory in the " +
			"repository that should own the mount point -- git cannot track an " +
			"empty directory, so put a placeholder file in it -- or correct the " +
			"target.",
	}
	shadowedTargets = refusal.Group{
		Rule: "target-shadowed",
		One: "a mount point cannot exist in the composed tree, because " +
			"something on the way to it is not a directory:",
		Many: "%d mount points cannot exist in the composed tree, because " +
			"something on the way to each is not a directory:",
		Detail: "Directories merge and files do not: a file in the code " +
			"repository where the workspace has a directory covers that whole " +
			"directory, and nothing under it is reachable in the composed tree. " +
			"Move or rename one of the two, or correct the target.",
	}
	targetTypes = refusal.Group{
		Rule: "target-type",
		One:  "a mount would put one kind of thing over another:",
		Many: "%d mounts would put one kind of thing over another:",
		Detail: "The kernel refuses that outright -- a directory binds onto a " +
			"directory and a file onto a file. Make the two ends the same kind of " +
			"thing.",
	}
)

// checkSource looks at what a mount would attach, and returns the type
// both ends have to be.
func (c *checker) checkSource(mount Mount) pathx.Type {
	switch mount.Role {
	case Store:
		// camp's own storage, created by camp before the mount, always a
		// directory. Nothing to refuse.
		return pathx.Dir
	case Artefact:
		// Generated in the prepare phase, so it does not exist yet. What it
		// contains is validated as hostile data once it does.
		return pathx.File
	}

	info, err := pathx.StatBeneath(c.cfg.Env, mount.SourceParts)
	switch {
	case err != nil:
		c.refused.Group(unreadableSources, "%s (%s): %v",
			mount.Source, describeOrigin(mount), err)
		return pathx.Absent
	case !info.Exists():
		c.refused.Group(missingSources, "%s (%s)", mount.Source, describeOrigin(mount))
		return pathx.Absent
	case info.Type == pathx.Symlink:
		c.refused.Group(symlinkSources, "%s (%s) points at %q",
			mount.Source, describeOrigin(mount), info.Link)
		return pathx.Absent
	case info.Type != pathx.Dir && info.Type != pathx.File:
		c.refused.Group(sourceTypes, "%s (%s) is a %s",
			mount.Source, describeOrigin(mount), info.Type)
		return pathx.Absent
	}
	return info.Type
}

// checkTarget asks whether the mount point exists in the tree as it will
// be at that moment, and whether it is the same kind of thing as the
// source.
func (c *checker) checkTarget(mount Mount, sourceType pathx.Type, tree *virtual) {
	targetType, err := tree.at(mount.Rel)
	switch {
	case errors.Is(err, pathx.ErrNotDirectory):
		c.refused.Group(shadowedTargets, "%q (%s): %v",
			mount.Rel.String(), describeOrigin(mount), err)
		return
	case err != nil:
		c.refused.Group(unreadableTargets, "%s (%s): %v",
			mount.Target, describeOrigin(mount), err)
		return
	}

	if targetType == pathx.Absent {
		// The exclude's own mount point is its own problem, with its own two
		// commands: it is missing because the code repository has no
		// .git/info at all, which is a different repair from a mount point
		// somebody forgot to commit. It can only ever fire once, so it needs
		// no group.
		if mount.Role == Artefact {
			c.refused.Add("target-missing", "%s", c.missingExcludeMessage(mount))
			return
		}
		c.refused.Group(missingTargets, "%q (%s) -- %s",
			mount.Rel.String(), describeOrigin(mount), mount.Target)
		return
	}
	if sourceType == pathx.Absent {
		return // already refused; do not pile a second message on the same cause
	}
	if targetType != sourceType {
		c.refused.Group(targetTypes, "%q (%s): the source %s is a %s and the "+
			"mount point in the composed tree is a %s",
			mount.Rel.String(), describeOrigin(mount), mount.Source, sourceType, targetType)
	}
}

func (c *checker) missingExcludeMessage(mount Mount) string {
	return fmt.Sprintf(
		"the generated exclude has nowhere to attach: %s does not exist in "+
			"the composed tree, because %s/.git/info/exclude is not there.\n"+
			"A repository initialised from an empty template has neither the "+
			"file nor the directory. camp will not create it -- it never writes "+
			"into a repository -- so run these two commands yourself:\n"+
			"  mkdir -p %s/.git/info\n  touch %s/.git/info/exclude",
		mount.Target, c.upper, c.upper, c.upper)
}

// checkTracked refuses a mount that would cover content the code
// repository tracks.
//
// Covering a tracked path makes git report those files deleted, and
// "git commit -a" records the deletion. The rule is stated over tracked
// content rather than over a list of names, so it needs no exceptions:
// .git and .git/info/exclude pass it automatically, because git tracks
// nothing under .git.
var trackedTargets = refusal.Group{
	Rule: "target-tracked-code",
	One:  "a mount would cover content the code repository tracks:",
	Many: "%d mounts would cover content the code repository tracks:",
	Detail: "Those files would vanish from the composed tree while staying in " +
		"the index, git would report them deleted, and 'git commit -a' would " +
		"record that deletion in the history. Move the mount, or move the " +
		"tracked content.",
}

func (c *checker) checkTracked(mount Mount) {
	if c.code == nil || mount.Rel.Empty() {
		return
	}
	tracked, err := c.code.TracksUnder(mount.Rel.String())
	if err != nil || len(tracked) == 0 {
		return
	}
	shown := tracked
	if len(shown) > 5 {
		shown = append(shown[:5:5], fmt.Sprintf("and %d more", len(tracked)-5))
	}
	c.refused.Group(trackedTargets, "%q (%s) would cover %s",
		mount.Rel.String(), describeOrigin(mount), strings.Join(shown, ", "))
}

// checkStoreNames refuses a target whose store would land on one of the
// files camp keeps for itself inside a storage directory.
//
// The specification does not raise this case: it places the scaffold
// manifest beside the target marker and outside every store, which is
// true for every target except one that is named after them. Rather than
// deciding quietly which of the two wins, camp refuses -- the collision
// is a configuration a person can change in one line.
var reservedTargets = refusal.Group{
	Rule: "target-reserved-name",
	One: "a target would put camp's own storage on top of a name camp keeps " +
		"for itself inside every storage directory:",
	Many: "%d targets would put camp's own storage on top of names camp keeps " +
		"for itself inside every storage directory:",
	Detail: "Those names are how camp tells its own objects from your " +
		"machine-local files, and which composition a leftover belonged to. " +
		"Give the mount another target.",
}

func (c *checker) checkStoreNames(built Plan) {
	for _, mount := range built.Mounts {
		if mount.Role != Store || mount.Rel.Empty() {
			continue
		}
		for _, reserved := range islands.Reserved {
			if mount.Rel.First() != reserved {
				continue
			}
			c.refused.Group(reservedTargets, "%q would land on %s",
				mount.Rel.String(), reserved)
		}
	}
}

// checkSourcePolicy refuses a writable mount that sources from a layer
// that must never be written.
var writableLower = refusal.Group{
	Rule: "source-under-lower",
	One:  "a writable mount sources from the workspace -- the lower layer:",
	Many: "%d writable mounts source from the workspace -- the lower layer:",
	Detail: "The lower is never written, by any route. That is not a " +
		"preference: while the composition is up the workspace is bound " +
		"read-only onto its own path, so such a mount would be writable in " +
		"name and refused by the kernel in practice, at the first write, in " +
		"the middle of a session.\n" +
		"If that content genuinely has to be writable, it belongs in a " +
		"repository of its own -- which is exactly why the record repository " +
		"was separated out -- with an empty mount-point directory committed in " +
		"the workspace for it to attach to.",
}

func (c *checker) checkSourcePolicy() {
	for index, step := range c.cfg.Steps {
		if step.Kind != config.MountRW {
			continue
		}
		for _, entry := range step.Entries {
			if entry.Source == nil || !c.cfg.IsLower(entry.Source.Repository) {
				continue
			}
			c.refused.Group(writableLower,
				"step %d mounts %s writable at %q, and %q is the workspace",
				index+1, entry.Source, entry.Target.String(), entry.Source.Repository)
		}
	}
}

// checkWorkdirFilesystem enforces the kernel's rule about the overlay's
// work directory.
//
// It has to be on the same filesystem as the upper, because the overlay
// moves files between the two by rename. camp's work directory lives
// under the environment root, so this only bites when the code repository
// is on a different filesystem than the environment -- a separate mount
// for the source tree, most often.
func (c *checker) checkWorkdirFilesystem(built Plan) {
	workDevice, workAt, ok := deviceOfNearest(built.Work)
	if !ok {
		return
	}
	upperDevice, _, ok := deviceOfNearest(c.upper)
	if !ok {
		return
	}
	if workDevice == upperDevice {
		return
	}
	c.refused.Add("workdir-filesystem",
		"the overlay's work directory would be %s, which is on a different "+
			"filesystem (%s is device %d) than the code repository %s (device "+
			"%d).\n"+
			"OverlayFS requires the two to be on one filesystem: it moves files "+
			"between them by rename, and a rename cannot cross a filesystem "+
			"boundary. Move env: onto the same filesystem as the code "+
			"repository, or move the repository.",
		built.OverlayWork, workAt, workDevice, c.upper, upperDevice)
}

// deviceOfNearest returns the device of a path, or of the nearest
// ancestor that exists -- camp's work directory has not been created yet
// when this runs.
func deviceOfNearest(path string) (uint64, string, bool) {
	for current := path; ; current = filepath.Dir(current) {
		var st syscall.Stat_t
		if err := syscall.Lstat(current, &st); err == nil {
			return uint64(st.Dev), current, true
		}
		if current == filepath.Dir(current) {
			return 0, "", false
		}
	}
}
