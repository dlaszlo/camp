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
// Two habits every call here keeps:
//
//   - LC_ALL=C. Command output is translated on this machine, and code
//     that decides by reading a message decides differently in another
//     language.
//   - --no-optional-locks. "git status" rewrites .git/index to refresh
//     its stat cache, and a reporting pass that quietly modifies the
//     repository it is reporting on is exactly what invariant 1 forbids.
package gitwire

import (
	"bytes"
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

// Open reports whether a directory is inside a git working tree, and
// returns a handle for reading it.
//
// A directory that is not is not an error: a composition need not be
// git-based at all, and camp says so rather than failing.
//
// Both facts come from one call. --is-inside-work-tree is the question
// that must be answered before anything else is asked -- it is false in a
// bare repository and inside a .git directory, where the reads below mean
// nothing -- and --show-prefix is the frame every later question needs.
// Asking them together also makes them one answer about one moment.
func Open(path string) (*Repo, bool) {
	repo := &Repo{Path: path}
	out, err := repo.run("rev-parse", "--is-inside-work-tree", "--show-prefix")
	if err != nil {
		return nil, false
	}
	inside, prefix, split := strings.Cut(string(out), "\n")
	if !split || inside != "true" {
		return nil, false
	}
	// --show-prefix prints the location with a trailing slash, and an empty
	// line at the root. Only the final newline is git's -- a directory name
	// may legally contain one -- so exactly one is removed, and then the
	// separator git adds.
	repo.prefix = strings.TrimSuffix(strings.TrimSuffix(prefix, "\n"), "/")
	return repo, true
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

// run executes one read-only git command and returns its raw output.
func (r *Repo) run(args ...string) ([]byte, error) {
	full := append([]string{"--no-optional-locks", "-C", r.Path}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANGUAGE=C")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s in %s: %w: %s",
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
	out, err := r.run("worktree", "list", "--porcelain")
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
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			current = &Worktree{Path: strings.TrimPrefix(line, "worktree ")}
		case current == nil:
			continue
		case strings.HasPrefix(line, "branch "):
			current.Branch = strings.TrimPrefix(
				strings.TrimPrefix(line, "branch "), "refs/heads/")
		case strings.HasPrefix(line, "prunable"):
			current.Prunable = true
			current.Reason = strings.TrimSpace(strings.TrimPrefix(line, "prunable"))
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
