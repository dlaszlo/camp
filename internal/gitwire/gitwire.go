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

// Repo is a git working tree, opened for reading.
type Repo struct {
	// Path is the working tree's root.
	Path string
}

// Open reports whether a directory is a git working tree, and returns a
// handle for reading it.
//
// A directory that is not one is not an error: a composition need not be
// git-based at all, and camp says so rather than failing.
func Open(path string) (*Repo, bool) {
	repo := &Repo{Path: path}
	if _, err := repo.run("rev-parse", "--is-inside-work-tree"); err != nil {
		return nil, false
	}
	return repo, true
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
	out, err := r.run("ls-files", "-z", "--full-name", "--", literal(path))
	if err != nil {
		return nil, err
	}
	return splitZero(out), nil
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
	paths := splitZero(out)
	sort.Strings(paths)
	return paths, nil
}

// ExcludeFile is the repository's own exclude, whose bytes the generated
// payload begins with.
func (r *Repo) ExcludeFile() string {
	return filepath.Join(r.Path, ".git", "info", "exclude")
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
