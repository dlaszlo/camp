package plan

import (
	"fmt"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/gitwire"
	"github.com/dlaszlo/camp/internal/inventory"
	"github.com/dlaszlo/camp/internal/islands"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/refusal"
)

// Prepare resolves a configuration into a plan and runs every check that
// can be made while nothing is mounted.
//
// That moment matters more than it looks. Before any mount exists, a
// repository can still be repaired by hand with an ordinary editor, and
// nothing anyone does can land in the wrong place. Every refusal that can
// be made here is made here, and the checks that genuinely cannot -- the
// ones about what generation produced, and the ones about what the kernel
// actually did -- are the only ones that happen later.
//
// The sequence is played through on paper, in its own order, over a
// virtual tree: each target is judged in the state its own step will
// really meet, so a target that an earlier mount supplies counts as
// present, and a later mount that would silently cover an earlier one is
// refused here rather than discovered as a missing file at midnight.
func Prepare(cfg config.Config, mode Mode) (Plan, refusal.List) {
	c := &checker{cfg: cfg, mode: mode}
	return c.run()
}

type checker struct {
	cfg     config.Config
	mode    Mode
	refused refusal.List

	lower string
	upper string
	live  string
	code  *gitwire.Repo
}

func (c *checker) run() (Plan, refusal.List) {
	c.checkNames()
	repositories := c.checkRepositories()
	c.lower = c.cfg.LowerPath()
	c.upper = c.cfg.UpperPath()

	// Without a usable lower and upper there is no tree to reason about,
	// and every check below would report a consequence of the same missing
	// directory. Everything found so far is reported; nothing is invented
	// on top of it.
	if !usable(repositories, c.cfg.Lower...) || !usable(repositories, c.cfg.Upper) {
		return Plan{}, c.refused
	}

	live, ok := c.checkLive(repositories)
	if !ok {
		return Plan{}, c.refused
	}
	c.live = live

	lowerRoot, upperRoot, ok := c.rootListings()
	if !ok {
		return Plan{}, c.refused
	}
	c.checkRootTypes(lowerRoot)

	if repo, isGit := gitwire.Open(c.upper); isGit {
		c.code = repo
	}

	built := Build(c.cfg, c.mode, c.live, Hash(c.live), lowerRoot, upperRoot)
	c.checkSequence(built)
	c.checkSourcePolicy()
	c.checkStoreNames(built)
	c.checkWorkdirFilesystem(built)
	c.refused.Extend(Gate(c.cfg, lowerRoot, upperRoot))

	// The accepted snapshot: a new name at the workspace root changes what
	// the derived binds protect and what the exclude covers, so it has to
	// be a change somebody decided rather than one that happened.
	problems, warnings := inventory.Check(c.cfg.CampDir(), inventory.Take(lowerRoot, upperRoot))
	c.refused.Extend(problems)
	built.Warnings = warnings

	return built, c.refused
}

func usable(repositories map[string]pathx.Info, names ...string) bool {
	for _, name := range names {
		info, ok := repositories[name]
		if !ok || info.Type != pathx.Dir {
			return false
		}
	}
	return len(names) > 0
}

// checkNames refuses any configured name that cannot be written down
// truthfully.
//
// A newline in a name cannot be expressed as a gitignore pattern at all:
// the attempt silently ignores the file it meant and hides two unrelated
// names instead. Every line-oriented report camp writes would be
// ambiguous too. So the name is refused rather than half-handled.
func (c *checker) checkNames() {
	report := func(kind, name string) {
		c.refused.Add("name-newline",
			"%s contains a line break: %q.\n"+
				"camp refuses such a name outright. It cannot be written as a "+
				"gitignore pattern -- the attempt silently ignores the file it meant "+
				"and hides two unrelated names instead -- and every report camp "+
				"prints a name into would become ambiguous. Rename it.", kind, name)
	}
	for _, repo := range c.cfg.Repositories {
		if pathx.HasNewline(repo.Name) {
			report("a repository name", repo.Name)
		}
		if pathx.HasNewline(repo.Path.String()) {
			report(fmt.Sprintf("the path of repository %q", repo.Name), repo.Path.String())
		}
	}
	for _, name := range c.cfg.AllowOverlap {
		if pathx.HasNewline(name) {
			report("an allow_overlap entry", name)
		}
	}
	for _, step := range c.cfg.Steps {
		for _, entry := range step.Entries {
			if pathx.HasNewline(entry.Target.String()) {
				report(fmt.Sprintf("a %s target", step.Kind), entry.Target.String())
			}
			if entry.Source != nil && pathx.HasNewline(entry.Source.Raw) {
				report(fmt.Sprintf("a %s source", step.Kind), entry.Source.Raw)
			}
		}
	}
}

// checkRepositories looks at each participant and at how they sit
// relative to one another.
func (c *checker) checkRepositories() map[string]pathx.Info {
	found := map[string]pathx.Info{}

	for _, repo := range c.cfg.Repositories {
		absolute := repo.Path.Join(c.cfg.Env)
		info, err := pathx.StatBeneath(c.cfg.Env, repo.Path.Components())
		switch {
		case err != nil:
			c.refused.Add("repository-unreadable",
				"the repository %q at %s could not be looked at: %v.\n"+
					"Every component of the path is opened without following a "+
					"symlink, because a bind mount follows them and one symlink in a "+
					"repository path could pull any directory on this machine into "+
					"the composition.", repo.Name, absolute, err)
			continue
		case !info.Exists():
			c.refused.Add("repository-missing",
				"the repository %q is declared at %s, and there is nothing there.\n"+
					"camp neither clones nor creates repositories. Either the path is "+
					"wrong, or the repository has not been checked out yet:\n"+
					"  git clone <url> %s", repo.Name, absolute, absolute)
			continue
		case info.Type == pathx.Symlink:
			c.refused.Add("repository-symlink",
				"the repository %q at %s is a symbolic link to %q.\n"+
					"camp follows no symlink in a mount operand. A link can be "+
					"repointed between the moment camp checks it and the moment the "+
					"kernel mounts it, and the check would then be about a different "+
					"directory than the mount. Write the real path in env: and the "+
					"repository path.", repo.Name, absolute, info.Link)
			continue
		case info.Type != pathx.Dir:
			c.refused.Add("repository-not-directory",
				"the repository %q at %s is a %s, not a directory.",
				repo.Name, absolute, info.Type)
			continue
		}
		found[repo.Name] = info
	}

	c.checkRepositoryIdentity(found)
	c.checkRepositoryNesting(found)
	return found
}

func (c *checker) checkRepositoryIdentity(found map[string]pathx.Info) {
	byIdentity := map[pathx.Identity]string{}
	for _, repo := range c.cfg.Repositories {
		info, ok := found[repo.Name]
		if !ok {
			continue
		}
		if other, clash := byIdentity[info.Ident]; clash {
			c.refused.Add("repository-same",
				"the repositories %q and %q are the same directory (%s and %s "+
					"resolve to inode %s).\n"+
					"They are compared by what the kernel says they are, not by how "+
					"they are spelled, because two different strings routinely name "+
					"one directory. One directory cannot play two parts in a "+
					"composition. Correct one of the two paths.",
				other, repo.Name,
				c.cfg.RepositoryPath(other), repo.Path.Join(c.cfg.Env), info.Ident)
			continue
		}
		byIdentity[info.Ident] = repo.Name
	}
}

func (c *checker) checkRepositoryNesting(found map[string]pathx.Info) {
	for _, outer := range c.cfg.Repositories {
		for _, inner := range c.cfg.Repositories {
			if outer.Name == inner.Name {
				continue
			}
			if _, ok := found[outer.Name]; !ok {
				continue
			}
			if _, ok := found[inner.Name]; !ok {
				continue
			}
			outerPath := outer.Path.Join(c.cfg.Env)
			innerPath := inner.Path.Join(c.cfg.Env)
			if outerPath != innerPath && pathx.Under(innerPath, outerPath) {
				c.refused.Add("repository-nested",
					"the repository %q (%s) is inside the repository %q (%s).\n"+
						"Nested repositories cannot be composed: the outer one's "+
						"content already contains the inner one, so mounting both makes "+
						"the same files appear twice with different rules, and a write "+
						"through one path would land somewhere the other path does not "+
						"agree with. Move one of them out.",
					inner.Name, innerPath, outer.Name, outerPath)
			}
		}
	}
}

// checkLive looks at the composed tree's directory.
//
// It has to exist, because a lock needs an inode to sit on and a bind
// cannot create its own mount point. It has to be empty, because an
// overlay laid over user content hides that content for the whole session
// and only down would ever name it -- by which time a day's work has been
// done on top of a tree that was quietly missing something.
func (c *checker) checkLive(found map[string]pathx.Info) (string, bool) {
	absolute := c.cfg.Live()
	info, err := pathx.StatBeneath(c.cfg.Env, c.cfg.Merged.Components())
	switch {
	case err != nil:
		c.refused.Add("live-unreadable",
			"the composed tree's directory %s could not be looked at: %v.", absolute, err)
		return "", false
	case !info.Exists():
		c.refused.Add("live-missing",
			"the composed tree's directory %s does not exist.\n"+
				"camp does not create it: it is where your work appears, and its "+
				"inode is what camp locks to stop a second composition being built "+
				"on it. Create it yourself:\n  mkdir %s", absolute, absolute)
		return "", false
	case info.Type == pathx.Symlink:
		c.refused.Add("live-symlink",
			"the composed tree's directory %s is a symbolic link to %q.\n"+
				"It has to be a real directory. camp locks the live directory's own "+
				"inode to guarantee one composition per tree, and a link has an "+
				"inode of its own that anything else could repoint.",
			absolute, info.Link)
		return "", false
	case info.Type != pathx.Dir:
		c.refused.Add("live-not-directory",
			"the composed tree's path %s is a %s, not a directory.", absolute, info.Type)
		return "", false
	}

	entries, err := pathx.ReadDirBeneath(c.cfg.Env, c.cfg.Merged.Components())
	if err != nil {
		c.refused.Add("live-unreadable",
			"the composed tree's directory %s could not be listed: %v.", absolute, err)
		return "", false
	}
	if len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name)
		}
		if len(names) > 8 {
			names = append(names[:8], fmt.Sprintf("and %d more", len(entries)-8))
		}
		c.refused.Add("live-not-empty",
			"the composed tree's directory %s is not empty. It holds: %s.\n"+
				"The overlay would be laid straight over that content and hide it "+
				"for the whole session -- you would notice at 'camp down', after a "+
				"day of work on a tree that was quietly missing something. Move that "+
				"content somewhere else, or point merged: at an empty directory.\n"+
				"If it is the residue of a composition that did not come down "+
				"cleanly, run 'camp status' first.",
			absolute, strings.Join(names, ", "))
		return "", false
	}

	for _, repo := range c.cfg.Repositories {
		if _, ok := found[repo.Name]; !ok {
			continue
		}
		repoPath := repo.Path.Join(c.cfg.Env)
		if pathx.Under(absolute, repoPath) {
			c.refused.Add("live-in-repository",
				"the composed tree %s is inside the repository %q (%s).\n"+
					"The composed tree is a view of the repositories; putting it "+
					"inside one of them makes that repository contain its own "+
					"reflection, and a write through the tree would land in a "+
					"directory the same tree is showing. Put merged: beside the "+
					"repositories, not in one.", absolute, repo.Name, repoPath)
			return "", false
		}
	}
	return absolute, true
}

func (c *checker) rootListings() ([]pathx.Info, []pathx.Info, bool) {
	lowerRoot, err := pathx.ReadDirBeneath(c.lower, nil)
	if err != nil {
		c.refused.Add("lower-unreadable",
			"the workspace repository %s could not be listed: %v.", c.lower, err)
		return nil, nil, false
	}
	upperRoot, err := pathx.ReadDirBeneath(c.upper, nil)
	if err != nil {
		c.refused.Add("upper-unreadable",
			"the code repository %s could not be listed: %v.", c.upper, err)
		return nil, nil, false
	}
	return lowerRoot, upperRoot, true
}

// checkRootTypes refuses a workspace root entry camp cannot protect.
//
// A read-only bind can stand over a directory or over a file. It cannot
// stand over a symlink, a socket, a FIFO or a device -- and a symlink at
// the root is worse than unsupported, because binding it would follow it
// out of the repository entirely.
func (c *checker) checkRootTypes(lowerRoot []pathx.Info) {
	covered := rootTargets(c.cfg)
	for _, entry := range lowerRoot {
		if covered[entry.Name] || c.cfg.AllowsOverlap(entry.Name) {
			continue
		}
		if pathx.HasNewline(entry.Name) {
			c.refused.Add("name-newline",
				"the workspace root entry %q contains a line break.\n"+
					"camp cannot write it as an exclude line -- the attempt silently "+
					"ignores that name and hides two unrelated ones instead -- so the "+
					"composition is refused rather than started with a hole in it. "+
					"Rename it in %s.", entry.Name, c.lower)
			continue
		}
		switch entry.Type {
		case pathx.Dir, pathx.File:
		case pathx.Symlink:
			c.refused.Add("root-entry-symlink",
				"the workspace root entry %s/%s is a symbolic link to %q.\n"+
					"camp protects every workspace root name with a read-only bind, "+
					"and a bind follows symlinks: binding this one would pull %q into "+
					"the composed tree and protect that instead. Replace it with a "+
					"real file or directory, or cover it with a mount target of its "+
					"own.", c.lower, entry.Name, entry.Link, entry.Link)
		default:
			c.refused.Add("root-entry-type",
				"the workspace root entry %s/%s is a %s.\n"+
					"camp binds a directory over a directory and a file over a file; "+
					"there is nothing it can do with a %s that would be honest. "+
					"Remove it from the workspace root, or cover it with a mount "+
					"target of its own.", c.lower, entry.Name, entry.Type, entry.Type)
		}
	}
}

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
