package plan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/envx"
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
	// warnings are what this pass found that stops nothing. They join the
	// ones the inventory finds, and the composing commands say them.
	warnings []string

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
	built.Environment = c.checkSessionEnvironment()
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
	built.Warnings = append(c.warnings, warnings...)

	return built, c.refused
}

func (c *checker) warn(format string, args ...any) {
	c.warnings = append(c.warnings, fmt.Sprintf(format, args...))
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

// checkSessionEnvironment resolves the declarations against the
// environment this command was started with.
//
// The configuration reader has already checked everything a file can be
// checked for on its own. What is left is the one question only the
// invoking environment can answer -- whether a referenced name is set --
// and the answer is needed here, while nothing is mounted, rather than in
// the middle of starting a session.
//
// The resolved bytes are dropped on the floor. They are needed by exactly
// one process, the one that starts the workload, and it resolves them
// again from its own inherited snapshot. What survives this function is
// what a report may print: the expression, rebuilt safely.
//
// The privileged mode resolves nothing: it starts no workload, so the
// section is announced there rather than applied, and refusing a
// reference nothing would read would be refusing a composition for a
// reason that does not apply to it.
func (c *checker) checkSessionEnvironment() []Variable {
	if c.mode != Namespace || !c.cfg.Session.Declares() {
		return nil
	}
	base := envx.NewBase(os.Environ(), c.live)

	declared := make([]Variable, 0, len(c.cfg.Session.Environment))
	for _, declaration := range c.cfg.Session.Environment {
		if _, err := declaration.Expr.Resolve(base); err != nil {
			var single refusal.R
			if errors.As(err, &single) {
				c.refused.Push(single)
			} else {
				c.refused.Add("environment-undefined", "%v", err)
			}
			continue
		}
		declared = append(declared, Variable{
			Name:      declaration.Name,
			Shown:     declaration.Expr.Display(c.live),
			Overrides: base.Has(declaration.Name),
		})
	}
	return declared
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
		c.refused.Group(newlineNames, "%s: %q", kind, name)
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

var newlineNames = refusal.Group{
	Rule: "name-newline",
	One:  "a configured name contains a line break:",
	Many: "%d configured names contain a line break:",
	Detail: "camp refuses such a name outright. It cannot be written as a " +
		"gitignore pattern -- the attempt silently ignores the file it meant " +
		"and hides two unrelated names instead -- and every report camp prints " +
		"a name into would become ambiguous. Rename it.",
}

// What a declared repository can be wrong about. A configuration written
// against a tree that has moved is wrong about several of them at once,
// and the reader fixes them in one pass over one file.
var (
	unreadableRepositories = refusal.Group{
		Rule: "repository-unreadable",
		One:  "a declared repository could not be looked at:",
		Many: "%d declared repositories could not be looked at:",
		Detail: "Every component of the path is opened without following a " +
			"symlink, because a bind mount follows them and one symlink in a " +
			"repository path could pull any directory on this machine into the " +
			"composition.",
	}
	missingRepositories = refusal.Group{
		Rule: "repository-missing",
		One:  "a declared repository is not there:",
		Many: "%d declared repositories are not there:",
		Detail: "camp neither clones nor creates repositories. Either the path is " +
			"wrong, or the repository has not been checked out yet:\n" +
			"  git clone <url> <the path above>",
	}
	symlinkRepositories = refusal.Group{
		Rule: "repository-symlink",
		One:  "a declared repository is a symbolic link:",
		Many: "%d declared repositories are symbolic links:",
		Detail: "camp follows no symlink in a mount operand. A link can be " +
			"repointed between the moment camp checks it and the moment the " +
			"kernel mounts it, and the check would then be about a different " +
			"directory than the mount. Write the real path in env: and the " +
			"repository path.",
	}
	repositoryTypes = refusal.Group{
		Rule: "repository-not-directory",
		One:  "a declared repository is not a directory:",
		Many: "%d declared repositories are not directories:",
		Detail: "A repository is a directory camp binds or overlays. Nothing else " +
			"can play the part.",
	}
	sameRepositories = refusal.Group{
		Rule: "repository-same",
		One:  "two declared repositories are the same directory:",
		Many: "%d pairs of declared repositories are the same directory:",
		Detail: "They are compared by what the kernel says they are, not by how " +
			"they are spelled, because two different strings routinely name one " +
			"directory. One directory cannot play two parts in a composition. " +
			"Correct one path of each pair.",
	}
	nestedRepositories = refusal.Group{
		Rule: "repository-nested",
		One:  "a declared repository is inside another:",
		Many: "%d declared repositories are inside another:",
		Detail: "Nested repositories cannot be composed: the outer one's content " +
			"already contains the inner one, so mounting both makes the same " +
			"files appear twice with different rules, and a write through one " +
			"path would land somewhere the other path does not agree with. Move " +
			"one of them out.",
	}
)

// checkRepositories looks at each participant and at how they sit
// relative to one another.
func (c *checker) checkRepositories() map[string]pathx.Info {
	found := map[string]pathx.Info{}

	for _, repo := range c.cfg.Repositories {
		absolute := repo.Path.Join(c.cfg.Env)
		info, err := pathx.StatBeneath(c.cfg.Env, repo.Path.Components())
		switch {
		case err != nil:
			c.refused.Group(unreadableRepositories, "%q at %s: %v",
				repo.Name, absolute, err)
			continue
		case !info.Exists():
			c.refused.Group(missingRepositories, "%q is declared at %s",
				repo.Name, absolute)
			continue
		case info.Type == pathx.Symlink:
			c.refused.Group(symlinkRepositories, "%q at %s points at %q",
				repo.Name, absolute, info.Link)
			continue
		case info.Type != pathx.Dir:
			c.refused.Group(repositoryTypes, "%q at %s is a %s",
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
			c.refused.Group(sameRepositories, "%q and %q: %s and %s both resolve "+
				"to inode %s", other, repo.Name,
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
				c.refused.Group(nestedRepositories, "%q (%s) is inside %q (%s)",
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
		// Not a refusal, and not created here either: a session creates it,
		// and planning executes nothing. A clone of an environment cannot
		// bring an empty directory with it -- git records no such thing --
		// so every fresh checkout would otherwise meet a refusal for the one
		// thing camp can safely make itself.
		//
		// What is still refused is a path whose parent does not exist. That
		// is a typo in merged:, and creating a tree of directories to match
		// one would build the composition somewhere nobody meant.
		if parent, err := pathx.Real(filepath.Dir(absolute)); err != nil || parent == "" {
			c.refused.Add("live-parent-missing",
				"the composed tree's directory %s cannot be created: %s does not "+
					"exist.\nmerged: names a directory inside the environment root, "+
					"and camp will not build a path of directories to reach one -- a "+
					"name that is not there is usually a typo, and the composition "+
					"would end up somewhere nobody meant. Create the parent, or "+
					"correct merged:.", absolute, filepath.Dir(absolute))
			return "", false
		}
		c.warn("the composed tree's directory %s does not exist yet; a session "+
			"creates it", absolute)
		return absolute, true
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
		// Refused, and the derivation carries on. This one is a precondition
		// for mounting, not a fault in the path: the directory is a real
		// directory and the plan built on it is exactly the plan a reader
		// wants to see. Stopping here returned an empty plan to every
		// caller, including the two that only describe -- so 'camp status'
		// refused to describe a composition that was up, in the words of the
		// refusal that recommends running it. Every caller that mounts stops
		// on a non-empty refusal list, which is what keeps this safe.
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
// What a workspace root entry can be that camp cannot protect. A
// workspace somebody has been working in has several of them at once --
// an editor's socket, a build directory's link -- so each is said once
// with every name it fired for.
var (
	newlineRootEntries = refusal.Group{
		Rule: "name-newline",
		One:  "a workspace root entry contains a line break:",
		Many: "%d workspace root entries contain a line break:",
		Detail: "camp cannot write such a name as an exclude line -- the attempt " +
			"silently ignores that name and hides two unrelated ones instead -- " +
			"so the composition is refused rather than started with a hole in it. " +
			"Rename it.",
	}
	symlinkRootEntries = refusal.Group{
		Rule: "root-entry-symlink",
		One:  "a workspace root entry is a symbolic link:",
		Many: "%d workspace root entries are symbolic links:",
		Detail: "camp protects every workspace root name with a read-only bind, " +
			"and a bind follows symlinks: binding one would pull whatever the " +
			"link points at into the composed tree and protect that instead. " +
			"Replace it with a real file or directory, or cover it with a mount " +
			"target of its own.",
	}
	rootEntryTypes = refusal.Group{
		Rule: "root-entry-type",
		One:  "a workspace root entry is something camp cannot bind:",
		Many: "%d workspace root entries are things camp cannot bind:",
		Detail: "camp binds a directory over a directory and a file over a file; " +
			"there is nothing it can do with a socket, a FIFO or a device that " +
			"would be honest. Remove it from the workspace root, or cover it with " +
			"a mount target of its own.",
	}
)

func (c *checker) checkRootTypes(lowerRoot []pathx.Info) {
	covered := rootTargets(c.cfg)
	for _, entry := range lowerRoot {
		if covered[entry.Name] || c.cfg.AllowsOverlap(entry.Name) {
			continue
		}
		if pathx.HasNewline(entry.Name) {
			c.refused.Group(newlineRootEntries, "%q in %s", entry.Name, c.lower)
			continue
		}
		switch entry.Type {
		case pathx.Dir, pathx.File:
		case pathx.Symlink:
			c.refused.Group(symlinkRootEntries, "%s/%s points at %q",
				c.lower, entry.Name, entry.Link)
		default:
			c.refused.Group(rootEntryTypes, "%s/%s is a %s",
				c.lower, entry.Name, entry.Type)
		}
	}
}
