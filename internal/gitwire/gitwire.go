// Package gitwire reads git. It never writes it.
//
// That is the whole contract, and it is the shape of the code rather than
// a promise: there is no function here that creates, modifies or removes
// anything in a repository. The previous design had one -- it wrote
// camp's block into .git/info/exclude -- and it was deleted, because
// camp never modifies a repository and an exclude that has to be written
// is an exclude that survives the session. The generated exclude is
// mounted over the composed tree's copy instead.
//
// Three habits every call here keeps:
//
//   - LC_ALL=C. Command output is translated on this machine, and code
//     that decides by reading a message decides differently in another
//     language.
//   - --no-optional-locks. "git status" rewrites .git/index to refresh
//     its stat cache, and a reporting pass that quietly modifies the
//     repository it is reporting on is exactly what invariant 1 forbids.
//   - The subprocess environment is built rather than inherited. -C names
//     the repository camp is asking about, and a dozen ambient variables
//     name a different one; see environment.
package gitwire

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dlaszlo/camp/internal/pathx"
)

// Repo is a directory inside a git working tree, opened for reading.
//
// The directory need not be the working tree's root. A composition's
// participants are directories, and whether one of them happens to be a
// repository root or a subdirectory of a larger repository is the user's
// arrangement, not something camp gets to require: a workspace can sit
// inside the environment's own repository and still be tracked content
// with a history behind it.
//
// So every question below is asked in the repository's frame -- pathspecs
// anchor at the root and --full-name answers in root-relative paths -- and
// answered in the opened directory's. A caller never learns which of the
// two it got, because for every question camp asks, the answer is the
// same either way.
type Repo struct {
	// Path is the directory that was opened.
	Path string
	// prefix is where Path sits inside the repository, relative to the
	// working tree's root and without a trailing slash. Empty when Path is
	// the root itself.
	prefix string
}

// Available reports whether git can be run at all.
//
// Open cannot answer this on its own: with no git on PATH every
// repository looks like "not a git working tree", and a caller that
// treats that as an ordinary answer would silently fall back to reading
// raw directory listings -- which is exactly the difference that has to
// be said out loud rather than absorbed.
func Available() error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git is not on PATH: %w", err)
	}
	return nil
}

// State is what a directory turned out to be.
//
// Three answers and not two, because the third used to be folded into the
// second and they mean opposite things. "This is not a working tree" is
// an ordinary answer -- a composition need not be git-based at all. "git
// could not tell me" is a check that did not run: no git on PATH, a
// damaged repository, a killed process, a directory that could not be
// read. Reading the second as the first turns every one of those into a
// composition that quietly skips the rule about covering tracked content,
// which is the rule that keeps 'git commit -a' from recording deletions
// nobody made.
type State int

const (
	// NotAWorkTree: git answered, and the answer is no. This is where a
	// bare repository and the inside of a .git directory land too.
	NotAWorkTree State = iota
	// InWorkTree: it is inside one, and the handle reads it.
	InWorkTree
	// Unreadable: git could not answer. Never the same as no.
	Unreadable
)

// Open reports what a directory is, and returns a handle for reading it.
//
// Both facts come from one call. --is-inside-work-tree is the question
// that must be answered before anything else is asked -- it is false in a
// bare repository and inside a .git directory, where the reads below mean
// nothing -- and --show-prefix is the frame every later question needs.
// Asking them together also makes them one answer about one moment.
//
// The two failures are told apart by git's own exit code and its message
// under LC_ALL=C, which is why every call in this package sets it: 128
// with "not a git repository" is git saying no, and anything else is git
// not answering. And git's no is looked at once more before it is
// believed, because a repository camp cannot read produces the same
// sentence -- see gitSaidNo.
func Open(path string) (*Repo, State, error) {
	repo := &Repo{Path: path}
	out, err := repo.run("rev-parse", "--is-inside-work-tree", "--show-prefix")
	if err != nil {
		if notARepository(err) {
			return gitSaidNo(path, err)
		}
		return nil, Unreadable, err
	}
	inside, prefix, split := strings.Cut(string(out), "\n")
	if !split || inside != "true" {
		return nil, NotAWorkTree, nil
	}
	// --show-prefix prints the location with a trailing slash, and an empty
	// line at the root. Only the final newline is git's -- a directory name
	// may legally contain one -- so exactly one is removed, and then the
	// separator git adds.
	repo.prefix = strings.TrimSuffix(strings.TrimSuffix(prefix, "\n"), "/")
	return repo, InWorkTree, nil
}

// notARepository reports whether git's failure was git saying no.
//
// Its exit code alone cannot: 128 is git's general fatal code. The
// message is matched too, and it is safe to match because every command
// here runs under LC_ALL=C -- which is the reason that habit exists.
func notARepository(err error) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 128 {
		return false
	}
	return strings.Contains(err.Error(), "not a git repository") ||
		strings.Contains(err.Error(), "this operation must be run in a work tree")
}

// gitSaidNo separates the two facts git spells the same way: there is no
// repository here, and there is one here that git could not read.
//
// git prints "fatal: not a git repository" for both. Measured on this
// machine: a directory holding a .git file whose gitdir does not exist
// gives "fatal: not a git repository: <the missing path>", and a
// directory holding an empty .git directory gives "fatal: not a git
// repository (or any parent up to mount point /)" -- character for
// character what a plain directory gives. Believing that word makes a
// damaged repository an ordinary "this is not a working tree", the
// tracked-content rule then has nothing to check, and a mount can cover
// tracked code while camp believes there is none. The tri-state exists
// because those two answers are opposite; this is where they are told
// apart.
//
// **The frame is the directory camp asked about, and not one component
// above it.** Git searches upward and camp does not, and the disagreement
// is deliberate in both directions. An ancestor's .git is somebody else's
// repository: camp was asked about this directory, and refusing a
// composition because a directory three levels up is damaged refuses it
// for a repository the configuration never named. Nor does walking up
// have a stopping rule that matches git's -- git halts at a filesystem
// boundary and at GIT_CEILING_DIRECTORIES -- so an upward scan would find
// .git entries git deliberately never considered and invent a damaged
// repository out of them. What the narrow frame gives up, plainly: a
// damaged repository whose root is *above* the directory camp opened
// still reads as "not a working tree". Every production caller opens a
// configured repository path, which is where .git is.
func gitSaidNo(path string, err error) (*Repo, State, error) {
	info, statErr := pathx.StatBeneath(path, []string{".git"})
	switch {
	case statErr != nil:
		// Whether there is a repository here has itself become a question
		// nobody answered, and an unanswered question is never the ordinary
		// no.
		return nil, Unreadable, fmt.Errorf(
			"git does not read %s as a repository, and whether it holds a .git "+
				"could not be looked at: %w (git said: %v)", path, statErr, err)
	case info.Exists():
		return nil, Unreadable, fmt.Errorf(
			"%s holds a .git %s and git does not read it as a repository: %w",
			path, info.Type, err)
	}
	return nil, NotAWorkTree, nil
}

// scoped turns a path relative to the opened directory into one relative
// to the repository root.
//
// This is the whole of the correction. A pathspec carrying ",top" anchors
// at the repository's root, so asking for ".claude" from a subdirectory
// asks about a ".claude" at the root -- which is usually not there, and
// git answers, correctly and uselessly, that nothing matches. That empty
// answer is indistinguishable from "the source contributes nothing", and
// camp would have gone on to mount no islands at all without a word.
func (r *Repo) scoped(path string) string {
	switch {
	case r.prefix == "":
		return path
	case path == "":
		return r.prefix
	default:
		return r.prefix + "/" + path
	}
}

// unscoped brings root-relative answers back into the opened directory's
// frame, and drops what falls outside it.
//
// Dropping is the point as much as trimming: a repository that holds the
// opened directory holds other things too, and a caller asking what this
// directory contains must not be handed a path that is not in it.
func (r *Repo) unscoped(paths []string) []string {
	if r.prefix == "" {
		return paths
	}
	prefix := r.prefix + "/"
	var kept []string
	for _, path := range paths {
		if rest := strings.TrimPrefix(path, prefix); rest != path {
			kept = append(kept, rest)
		}
	}
	return kept
}

// environment is what every git subprocess here runs with. It is built,
// never inherited.
//
// The repository camp asks about is named by -C and by nothing else --
// and git takes that name straight back out of the environment. Three
// measurements on this machine, all against a real checkout that -C
// named:
//
//   - GIT_DIR at a path that does not exist: exit 128, "fatal: not a git
//     repository", which is git's own word for no and used to arrive here
//     as "this is not a working tree";
//   - GIT_WORK_TREE somewhere else: --is-inside-work-tree prints false and
//     exits 0 -- an ordinary no, with no failure anywhere to notice;
//   - GIT_INDEX_FILE at a path that does not exist: still a working tree,
//     and ls-files prints nothing and exits 0 -- an empty tracked set,
//     which is how camp reads "this mount covers nothing tracked".
//
// Each of those switches off the one rule camp asks git about, and two of
// them do it without an error anywhere. GIT_COMMON_DIR,
// GIT_OBJECT_DIRECTORY, GIT_CEILING_DIRECTORIES and the GIT_CONFIG_*
// family reach the same place by their own routes.
//
// **An allowlist, not a denylist.** A denylist is a list of the variables
// that existed on the day it was written: the next git release adds one,
// and the denylist goes on letting it through in silence -- and silence
// here means the guard is off. An allowlist that misses something new
// costs a variable git no longer receives, which is a bug somebody sees.
// What survives, one reason each:
//
//   - PATH -- how git itself is found, and the same PATH Available looked
//     in, so the git that runs is the git camp checked for. It names no
//     repository.
//   - HOME and XDG_CONFIG_HOME -- where git finds the user's own
//     configuration, which is the configuration the user's own git reads
//     in that repository; camp's answer to "what does this repository
//     track" should be git's answer, not a different one. Both or
//     neither: keeping one of the two would read a configuration nobody
//     has. Neither is a discovery control -- measured, core.worktree in a
//     global configuration is ignored, and honoured only from the
//     repository's own config -- and what does live there is
//     safe.directory, so dropping them would make camp refuse a
//     repository whose owner has explicitly allowed it.
//
// Nothing else, and no GIT_* at all, not even a GIT_PAGER or a GIT_TRACE
// that steers nothing: an allowlist that starts making exceptions for the
// ones that look harmless is a denylist again.
//
// GIT_* is also not the whole of what steers git, which is the other
// reason the rule is what survives rather than what is caught: LD_PRELOAD
// and LD_LIBRARY_PATH put code inside the process before main, and TMPDIR
// says where it writes. They go with everything else, for free.
//
// LC_ALL and LANGUAGE are camp's own, set here rather than inherited, and
// now the only locale settings the process has at all. notARepository is
// what they are load-bearing for.
func environment() []string {
	kept := []string{"PATH", "HOME", "XDG_CONFIG_HOME"}
	env := make([]string, 0, len(kept)+2)
	for _, name := range kept {
		if value, found := os.LookupEnv(name); found {
			env = append(env, name+"="+value)
		}
	}
	return append(env, "LC_ALL=C", "LANGUAGE=C")
}

// run executes one read-only git command and returns its raw output.
func (r *Repo) run(args ...string) ([]byte, error) {
	full := append([]string{"--no-optional-locks", "-C", r.Path}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = environment()
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s in %s: %w: %s",
			strings.Join(args, " "), r.Path, err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// fed executes one read-only git command with names on its standard
// input, and returns its raw output.
//
// It exists for check-ignore, which is the one command here that takes a
// list rather than a pathspec -- and which accepts the null-separated
// form only when the list arrives on stdin. That form is not a
// nicety: a filename may contain a newline, and the line-separated
// output quotes such a name instead of printing it, so a reader splitting
// on newlines would either mis-read the name or have to un-quote it.
// Null-separated is the same rule this package reads git's other output
// by.
func (r *Repo) fed(stdin []byte, args ...string) ([]byte, error) {
	full := append([]string{"--no-optional-locks", "-C", r.Path}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = environment()
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("git %s in %s: %w: %s",
			strings.Join(args, " "), r.Path, err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// literal builds a pathspec that means exactly this path: no wildcard
// interpretation, anchored at the repository root.
//
// Without it a tracked file called "a[b]" would be read as a character
// class, and the check that is supposed to protect it would silently
// examine a different path.
func literal(path string) string { return ":(literal,top)" + path }

func splitZero(data []byte) []string {
	var out []string
	for _, field := range bytes.Split(data, []byte{0}) {
		if len(field) > 0 {
			out = append(out, string(field))
		}
	}
	return out
}

// Ignored reports which of these names the repository's ignore rules
// cover.
//
// What it is for, and what it deliberately does not do: the accepted
// snapshot records what is in a repository's root rather than what git
// tracks, because the composed tree shows what is on disk -- an ignored
// file at a root still appears in the tree and still collides with a name
// on the other side. So this never filters anything. It answers the
// question a person asks when camp reports a new root entry and they have
// to choose between accepting it and removing it: is this content, or is
// it something a tool left?
//
// A tracked path is not reported, which is git's own rule and the right
// one here: ignore patterns do not apply to what is tracked, so a tracked
// file matching one is content that somebody added deliberately.
//
// A nil repository ignores nothing. There is no work tree to ask, which
// is an answer rather than a failure -- the composition is not required
// to be a git repository at all.
func (r *Repo) Ignored(names []string) (map[string]bool, error) {
	covered := map[string]bool{}
	if r == nil || len(names) == 0 {
		return covered, nil
	}
	var asked bytes.Buffer
	for _, name := range names {
		asked.WriteString(name)
		asked.WriteByte(0)
	}
	out, err := r.fed(asked.Bytes(), "check-ignore", "-z", "--stdin")
	if err != nil {
		// One is "nothing matched", which is an answer and not a failure.
		// git reserves it for exactly that, and anything above it is a real
		// error worth reporting.
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return covered, nil
		}
		return nil, err
	}
	for _, name := range splitZero(out) {
		covered[name] = true
	}
	return covered, nil
}

// TracksUnder reports whether anything the repository tracks resolves at
// or under a path.
//
// This is the check behind the rule that no mount may cover tracked code:
// covering a tracked path makes git report those files deleted, and
// "git commit -a" records it. The rule needs no exception list -- .git
// and .git/info/exclude pass automatically, because git tracks nothing
// under .git.
func (r *Repo) TracksUnder(path string) ([]string, error) {
	out, err := r.run("ls-files", "-z", "--full-name", "--", literal(r.scoped(path)))
	if err != nil {
		return nil, err
	}
	return r.unscoped(splitZero(out)), nil
}

// Contributes returns the entries a repository contributes at a
// directory: the distinct first components of everything it tracks there,
// each with the type the working tree actually has.
//
// Derived from tracked content, not from the raw listing. The raw listing
// would hand out islands to the source's own runtime junk -- its
// settings.local.json, its lock files -- which is precisely what the
// islands mount exists to keep out of the composed tree.
func (r *Repo) Contributes(relative string) ([]pathx.Info, error) {
	tracked, err := r.TracksUnder(relative)
	if err != nil {
		return nil, err
	}

	prefix := relative + "/"
	seen := map[string]bool{}
	var names []string
	for _, path := range tracked {
		rest := path
		if relative != "" {
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			rest = strings.TrimPrefix(path, prefix)
		}
		name, _, _ := strings.Cut(rest, "/")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)

	base := filepath.Join(r.Path, relative)
	entries := make([]pathx.Info, 0, len(names))
	for _, name := range names {
		info, err := pathx.StatBeneath(base, []string{name})
		if err != nil {
			return nil, err
		}
		entries = append(entries, info)
	}
	return entries, nil
}

// Indexed returns every path in the index, with its stage.
//
// This is the scan that sees a forced add. "git add -f" through the
// composed tree stages the bytes of a workspace file and leaves an
// indexed path with no file in the raw working tree at all, so a scan for
// untracked files is structurally blind to exactly the leak it is meant
// to find.
func (r *Repo) Indexed() ([]string, error) {
	out, err := r.run("ls-files", "-z", "--stage", "--full-name")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, record := range splitZero(out) {
		// "<mode> <object> <stage>\t<path>"
		if _, path, found := strings.Cut(record, "\t"); found {
			paths = append(paths, path)
		}
	}
	paths = r.unscoped(paths)
	sort.Strings(paths)
	return paths, nil
}

// Untracked returns the paths git can see but does not track, ignored
// files excluded.
func (r *Repo) Untracked() ([]string, error) {
	out, err := r.run("ls-files", "-z", "--others", "--exclude-standard", "--full-name")
	if err != nil {
		return nil, err
	}
	paths := r.unscoped(splitZero(out))
	sort.Strings(paths)
	return paths, nil
}

// Worktree is one registered worktree.
type Worktree struct {
	Path     string
	Branch   string
	Prunable bool
	Reason   string
}

// Worktrees lists what the repository has registered.
func (r *Repo) Worktrees() ([]Worktree, error) {
	// -z, and the reason is the same one that makes every comparison here
	// byte-oriented: a checkout's path may contain a space, a tab, a
	// newline or a quote, and git's line-oriented porcelain quotes such a
	// path -- or, for a newline, splits one record across what looks like
	// two. The NUL form has no such case: one field per NUL, one empty
	// field between records, and the path exactly as it is on disk.
	out, err := r.run("worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}

	var found []Worktree
	var current *Worktree
	flush := func() {
		if current != nil {
			found = append(found, *current)
			current = nil
		}
	}
	for _, field := range strings.Split(string(out), "\x00") {
		switch {
		case field == "":
			// The empty field between records, and the tail after the last
			// one.
			flush()
		case strings.HasPrefix(field, "worktree "):
			flush()
			current = &Worktree{Path: strings.TrimPrefix(field, "worktree ")}
		case current == nil:
			continue
		case strings.HasPrefix(field, "branch "):
			current.Branch = strings.TrimPrefix(
				strings.TrimPrefix(field, "branch "), "refs/heads/")
		case strings.HasPrefix(field, "prunable"):
			current.Prunable = true
			current.Reason = strings.TrimSpace(strings.TrimPrefix(field, "prunable"))
		}
	}
	flush()
	return found, nil
}

// WorktreesUnder returns the worktrees registered at a path inside the
// composed tree.
//
// These are the ones whose registration dies with the composition: git
// stores a worktree's git directory as an absolute path and compares it
// as a string, so a worktree created through the live tree records the
// live path on both sides, and after down neither pointer resolves. The
// checkout's files are intact; git simply cannot see them any more.
func WorktreesUnder(worktrees []Worktree, live string) []Worktree {
	var inside []Worktree
	for _, worktree := range worktrees {
		if pathx.Under(worktree.Path, live) && worktree.Path != live {
			inside = append(inside, worktree)
		}
	}
	return inside
}
