package plan

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/islands"
	"github.com/dlaszlo/camp/internal/pathx"
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

func (c *checker) checkOrder(mount Mount, placed []Mount) {
	for _, earlier := range placed {
		switch {
		case earlier.Rel.Equal(mount.Rel):
			c.refused.Add("target-duplicate",
				"two mounts declare the same target, %q: %s, and then %s.\n"+
					"The second would cover the first completely, so the first would "+
					"exist in the mount table and be reachable by nothing. camp will "+
					"not mount something it knows is unreachable. Keep the one you "+
					"meant.",
				mount.Rel.String(), describeOrigin(earlier), describeOrigin(mount))
		case earlier.Rel.Inside(mount.Rel):
			c.refused.Add("target-nested",
				"the mount at %q (%s) is declared before the mount at %q (%s), "+
					"and the second contains the first.\n"+
					"Mounted in that order the second would silently cover the first: "+
					"it would still be listed in the kernel's mount table, and no path "+
					"would reach it. Parent first, then child -- swap the two.\n"+
					"The order in steps: is the mount order, which is exactly why it "+
					"can be checked before anything is mounted.",
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
		c.refused.Add("source-unreadable",
			"the mount source %s could not be looked at: %v.", mount.Source, err)
		return pathx.Absent
	case !info.Exists():
		c.refused.Add("source-missing",
			"the mount source %s does not exist (%s).\n"+
				"camp creates nothing inside a repository. Either the source is "+
				"wrong, or the directory has to be added to the repository that owns "+
				"it and committed -- git cannot track an empty directory, so a "+
				"mount point needs a placeholder file in it.",
			mount.Source, describeOrigin(mount))
		return pathx.Absent
	case info.Type == pathx.Symlink:
		c.refused.Add("source-symlink",
			"the mount source %s is a symbolic link to %q.\n"+
				"A bind mount follows symlinks, so this one would attach %q to the "+
				"composed tree -- possibly a directory nowhere near the repository "+
				"it was supposed to come from. Point the source at the real path.",
			mount.Source, info.Link, info.Link)
		return pathx.Absent
	case info.Type != pathx.Dir && info.Type != pathx.File:
		c.refused.Add("source-type",
			"the mount source %s is a %s. camp binds directories and regular "+
				"files, nothing else.", mount.Source, info.Type)
		return pathx.Absent
	}
	return info.Type
}

// checkTarget asks whether the mount point exists in the tree as it will
// be at that moment, and whether it is the same kind of thing as the
// source.
func (c *checker) checkTarget(mount Mount, sourceType pathx.Type, tree *virtual) {
	targetType, err := tree.at(mount.Rel)
	if err != nil {
		c.refused.Add("target-unreadable",
			"the mount point %s could not be looked at: %v.", mount.Target, err)
		return
	}

	if targetType == pathx.Absent {
		c.refused.Add("target-missing", "%s", c.missingTargetMessage(mount))
		return
	}
	if sourceType == pathx.Absent {
		return // already refused; do not pile a second message on the same cause
	}
	if targetType != sourceType {
		c.refused.Add("target-type",
			"the mount at %q would put a %s over a %s: the source %s is a %s "+
				"and the mount point in the composed tree is a %s.\n"+
				"The kernel refuses that outright -- a directory binds onto a "+
				"directory and a file onto a file. Make the two ends the same kind "+
				"of thing.",
			mount.Rel.String(), sourceType, targetType, mount.Source, sourceType, targetType)
	}
}

func (c *checker) missingTargetMessage(mount Mount) string {
	target := mount.Target
	if mount.Role == Artefact {
		return fmt.Sprintf(
			"the generated exclude has nowhere to attach: %s does not exist in "+
				"the composed tree, because %s/.git/info/exclude is not there.\n"+
				"A repository initialised from an empty template has neither the "+
				"file nor the directory. camp will not create it -- it never writes "+
				"into a repository -- so run these two commands yourself:\n"+
				"  mkdir -p %s/.git/info\n  touch %s/.git/info/exclude",
			target, c.upper, c.upper, c.upper)
	}
	return fmt.Sprintf(
		"the mount point %q does not exist in the composed tree (%s), so %s "+
			"has nothing to attach to.\n"+
			"A bind mount cannot create its own mount point, and by the time this "+
			"mount happens the tree underneath it is read-only, so camp cannot "+
			"create one either. Commit an empty directory in the repository that "+
			"should own the mount point -- git cannot track an empty directory, so "+
			"put a placeholder file in it -- or correct the target.",
		mount.Rel.String(), target, describeOrigin(mount))
}

// checkTracked refuses a mount that would cover content the code
// repository tracks.
//
// Covering a tracked path makes git report those files deleted, and
// "git commit -a" records the deletion. The rule is stated over tracked
// content rather than over a list of names, so it needs no exceptions:
// .git and .git/info/exclude pass it automatically, because git tracks
// nothing under .git.
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
	c.refused.Add("target-tracked-code",
		"the mount at %q (%s) would cover content the code repository "+
			"tracks: %s.\n"+
			"Those files would vanish from the composed tree while staying in the "+
			"index, git would report them deleted, and 'git commit -a' would "+
			"record that deletion in the history. Move the mount, or move the "+
			"tracked content.",
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
func (c *checker) checkStoreNames(built Plan) {
	for _, mount := range built.Mounts {
		if mount.Role != Store || mount.Rel.Empty() {
			continue
		}
		for _, reserved := range islands.Reserved {
			if mount.Rel.First() != reserved {
				continue
			}
			c.refused.Add("target-reserved-name",
				"the target %q would put camp's own storage on top of %s, which "+
					"camp keeps for itself inside every storage directory: it is "+
					"how camp tells its own objects from your machine-local files "+
					"and which composition a leftover belonged to.\n"+
					"Give the mount another target.", mount.Rel.String(), reserved)
		}
	}
}

// checkSourcePolicy refuses a writable mount that sources from a layer
// that must never be written.
func (c *checker) checkSourcePolicy() {
	for index, step := range c.cfg.Steps {
		if step.Kind != config.MountRW {
			continue
		}
		for _, entry := range step.Entries {
			if entry.Source == nil || !c.cfg.IsLower(entry.Source.Repository) {
				continue
			}
			c.refused.Add("source-under-lower",
				"step %d mounts %s writable at %q, and %q is the workspace -- "+
					"the lower layer.\n"+
					"The lower is never written, by any route. That is not a "+
					"preference: while the composition is up the workspace is bound "+
					"read-only onto its own path, so this mount would be writable in "+
					"name and refused by the kernel in practice, at the first write, "+
					"in the middle of a session.\n"+
					"If that content genuinely has to be writable, it belongs in a "+
					"repository of its own -- which is exactly why the record "+
					"repository was separated out -- with an empty mount-point "+
					"directory committed in the workspace for it to attach to.",
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
