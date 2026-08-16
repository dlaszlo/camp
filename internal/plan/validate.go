package plan

import (
	"fmt"
	"strings"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/gitwire"
	"github.com/dlaszlo/camp/internal/inventory"
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
