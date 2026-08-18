package gitwire_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/gitwire"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/testenv"
)

// The same directory, in the two shapes a real environment produces it:
// as a repository of its own, and as one directory inside a larger
// repository that holds the whole environment. Every question camp asks
// has to give the same answer in both, because the arrangement of the
// repositories is the user's decision and none of camp's questions is
// about it.
//
// What is in the fixture, and why each entry is there:
//
//	.claude/settings.json        tracked -- an island
//	.claude/agents/reviewer.md   tracked -- makes .claude/agents an island
//	.claude/settings.local.json  ignored -- must never become an island
//	CLAUDE.md                    tracked -- a root entry, not under .claude
//
// stray.txt is written after the commit instead, because it has to stay
// untracked and committing is what the builders do next.
func fixture(t *testing.T, directory string) {
	t.Helper()
	testenv.Write(t, filepath.Join(directory, ".gitignore"),
		"/.claude/settings.local.json\n")
	testenv.Write(t, filepath.Join(directory, ".claude", "settings.json"), "{}\n")
	testenv.Write(t, filepath.Join(directory, ".claude", "agents", "reviewer.md"), "x\n")
	testenv.Write(t, filepath.Join(directory, ".claude", "settings.local.json"), "{}\n")
	testenv.Write(t, filepath.Join(directory, "CLAUDE.md"), "instructions\n")
}

// atTheRoot builds the fixture as a repository of its own.
func atTheRoot(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(testenv.Root(t), "workspace")
	testenv.GitRepo(t, directory)
	fixture(t, directory)
	testenv.Commit(t, directory, "the workspace")
	testenv.Write(t, filepath.Join(directory, "stray.txt"), "not committed\n")
	return directory
}

// insideALargerRepository builds the same fixture as a subdirectory, with
// content beside and above it that the answers must not mention.
func insideALargerRepository(t *testing.T) string {
	t.Helper()
	outer := filepath.Join(testenv.Root(t), "env")
	testenv.GitRepo(t, outer)
	directory := filepath.Join(outer, "workspace")
	fixture(t, directory)
	testenv.Write(t, filepath.Join(outer, "README.md"), "the environment\n")
	testenv.Write(t, filepath.Join(outer, "notes", "design.md"), "why\n")
	testenv.Commit(t, outer, "the environment")
	testenv.Write(t, filepath.Join(directory, "stray.txt"), "not committed\n")
	testenv.Write(t, filepath.Join(outer, "outside.txt"), "not committed\n")
	return directory
}

func shapes(t *testing.T) map[string]func(*testing.T) string {
	t.Helper()
	return map[string]func(*testing.T) string{
		"as a repository of its own": atTheRoot,
		"inside a larger repository": insideALargerRepository,
	}
}

func open(t *testing.T, directory string) *gitwire.Repo {
	t.Helper()
	repo, state, err := gitwire.Open(directory)
	if state != gitwire.InWorkTree {
		t.Fatalf("%s was not recognised as being in a git working tree: %v", directory, err)
	}
	return repo
}

func names(entries []pathx.Info) []string {
	found := make([]string, 0, len(entries))
	for _, entry := range entries {
		found = append(found, string(entry.Type)+" "+entry.Name)
	}
	return found
}

func mustEqual(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%s:\n  got  %v\n  want %v", what, got, want)
	}
}

// The islands a source contributes are the same either way. This is the
// answer the whole correction exists for: before it, a source inside a
// larger repository was asked about a path at that repository's root,
// found nothing there, and camp mounted no islands at all without a word.
func TestASourceContributesTheSameWhereverTheRepositoryRootIs(t *testing.T) {
	for shape, build := range shapes(t) {
		t.Run(shape, func(t *testing.T) {
			repo := open(t, build(t))

			entries, err := repo.Contributes(".claude")
			if err != nil {
				t.Fatalf("asking what the source contributes: %v", err)
			}
			mustEqual(t, "the islands at .claude", names(entries),
				[]string{"directory agents", "file settings.json"})
		})
	}
}

// The machine-local file is left out because nothing tracks it -- not
// because camp knows the name. The store covers the whole directory and
// only contributed entries are mounted back, so an untracked name simply
// never gets an island.
func TestAnIgnoredEntryIsNeverAnIsland(t *testing.T) {
	for shape, build := range shapes(t) {
		t.Run(shape, func(t *testing.T) {
			repo := open(t, build(t))

			entries, err := repo.Contributes(".claude")
			if err != nil {
				t.Fatalf("asking what the source contributes: %v", err)
			}
			for _, entry := range entries {
				if entry.Name == "settings.local.json" {
					t.Error("settings.local.json became an island; it is machine-local " +
						"and no repository tracks it")
				}
			}
		})
	}
}

// TracksUnder answers in the opened directory's frame, whatever frame git
// answered in. This is the check behind the rule that no mount may cover
// tracked code, and a path in the wrong frame would compare against the
// composed tree's paths and never match -- a protection switched off in
// silence.
func TestTracksUnderAnswersInTheOpenedDirectorysFrame(t *testing.T) {
	for shape, build := range shapes(t) {
		t.Run(shape, func(t *testing.T) {
			repo := open(t, build(t))

			tracked, err := repo.TracksUnder(".claude")
			if err != nil {
				t.Fatalf("asking what is tracked: %v", err)
			}
			mustEqual(t, "what is tracked under .claude", tracked,
				[]string{".claude/agents/reviewer.md", ".claude/settings.json"})
		})
	}
}

// Everything a repository holds beside the opened directory belongs to
// somebody else's question. A caller asking what this directory contains
// must not be handed a path that is not in it.
func TestTheAnswersStayInsideTheOpenedDirectory(t *testing.T) {
	repo := open(t, insideALargerRepository(t))

	indexed, err := repo.Indexed()
	if err != nil {
		t.Fatalf("reading the index: %v", err)
	}
	mustEqual(t, "the indexed paths", indexed, []string{
		".claude/agents/reviewer.md",
		".claude/settings.json",
		".gitignore",
		"CLAUDE.md",
	})

	untracked, err := repo.Untracked()
	if err != nil {
		t.Fatalf("reading the untracked paths: %v", err)
	}
	mustEqual(t, "the untracked paths", untracked, []string{"stray.txt"})
}

// The whole repository is still readable when the opened directory is its
// root: the frame correction must not have turned into a filter that
// drops things.
func TestAtTheRootEverythingIsStillAnswered(t *testing.T) {
	repo := open(t, atTheRoot(t))

	indexed, err := repo.Indexed()
	if err != nil {
		t.Fatalf("reading the index: %v", err)
	}
	mustEqual(t, "the indexed paths", indexed, []string{
		".claude/agents/reviewer.md",
		".claude/settings.json",
		".gitignore",
		"CLAUDE.md",
	})
}

// A directory in no repository at all is not an error and not a git
// repository either -- it is the answer that sends the caller to the raw
// listing, which is a usable answer and camp says so where it is noticed.
//
// GIT_CEILING_DIRECTORIES makes "there is no repository above this" true
// by construction: the scratch root may itself sit under a checkout on
// whatever machine runs the tests, and the fact under test is about the
// directory, not about that machine.
func TestADirectoryInNoRepositoryIsNotOpened(t *testing.T) {
	root := testenv.Root(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", root)

	directory := filepath.Join(root, "plain")
	testenv.MkDir(t, directory)
	fixture(t, directory)

	if _, state, err := gitwire.Open(directory); state != gitwire.NotAWorkTree {
		t.Fatalf("%s is in no git working tree and answered %v (%v)", directory, state, err)
	}
}

// A bare repository has no working tree, so every read below Open would
// be meaningless there. Open has to say no, and this is why the question
// --is-inside-work-tree is still asked rather than replaced by
// --show-prefix alone: measured, --show-prefix *succeeds* in a bare
// repository and prints an empty line, so the frame question on its own
// would have opened one.
func TestABareRepositoryIsNotAWorkingTree(t *testing.T) {
	root := testenv.Root(t)
	directory := filepath.Join(root, "bare.git")
	command := exec.Command("git", "init", "--quiet", "--bare", directory)
	command.Env = append(os.Environ(), "LC_ALL=C",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("creating a bare repository: %v\n%s", err, out)
	}

	if _, state, err := gitwire.Open(directory); state != gitwire.NotAWorkTree {
		t.Fatalf("%s has no working tree and answered %v (%v)", directory, state, err)
	}
}

// git failing to answer and git answering "no" are opposite facts, and
// they used to be the same value.
//
// Everything camp asks git guards one rule: no mount may cover content
// the code repository tracks, because covering it makes git report those
// files deleted and 'git commit -a' records the deletion. A composition
// where git could not run must not go ahead with that rule quietly not
// checked -- so the two answers are told apart here, at the one place
// that can tell them apart.
func TestGitFailingIsNotTheSameAsGitSayingNo(t *testing.T) {
	root := testenv.Root(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", root)

	plain := filepath.Join(root, "plain")
	testenv.MkDir(t, plain)
	if _, state, err := gitwire.Open(plain); state != gitwire.NotAWorkTree {
		t.Errorf("a plain directory answered %v (%v)", state, err)
	}

	// The same directory, with no git to ask. Nothing about the directory
	// changed; what changed is that the question cannot be answered.
	t.Setenv("PATH", filepath.Join(root, "nothing-here"))
	repo, state, err := gitwire.Open(plain)
	if state != gitwire.Unreadable {
		t.Fatalf("with no git on PATH the answer was %v, wanted Unreadable", state)
	}
	if repo != nil {
		t.Error("a handle came back for a directory git could not read")
	}
	if err == nil {
		t.Error("nothing said why it could not be read")
	}
}

// A repository that is there and cannot be read is not the same as a
// directory that is no repository.
//
// The two states are already told apart where git is missing entirely.
// This is the other half, and it is the half a real machine produces: a
// repository whose directory camp cannot enter, one whose index cannot
// be read, and one whose .git is damaged. The first two are camp's to
// tell apart. The third is not, and that is measured here rather than
// assumed.
func TestARepositoryThatCannotBeReadIsNotGitSayingNo(t *testing.T) {
	build := func(t *testing.T) string {
		t.Helper()
		root := testenv.Root(t)
		t.Setenv("GIT_CEILING_DIRECTORIES", root)
		directory := filepath.Join(root, "workspace")
		testenv.GitRepo(t, directory)
		fixture(t, directory)
		testenv.Commit(t, directory, "the workspace")
		return directory
	}

	// git cannot even change into it, so it never reaches the question.
	t.Run("a directory camp cannot enter", func(t *testing.T) {
		directory := build(t)
		if err := os.Chmod(directory, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(directory, 0o755) })

		repo, state, err := gitwire.Open(directory)
		if state != gitwire.Unreadable {
			t.Fatalf("a directory that cannot be entered answered %v (%v)", state, err)
		}
		if repo != nil {
			t.Error("a handle came back for a directory git could not read")
		}
		if err == nil {
			t.Error("nothing said why it could not be read")
		}
	})

	// The frame question is answered from .git and needs no index, so this
	// one is a working tree -- and the question that follows fails. What
	// must not happen is that failure arriving as an empty list, which is
	// the answer "this mount covers nothing tracked".
	t.Run("an index camp cannot read", func(t *testing.T) {
		directory := build(t)
		index := filepath.Join(directory, ".git", "index")
		if err := os.Chmod(index, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(index, 0o644) })

		repo, state, err := gitwire.Open(directory)
		if state != gitwire.InWorkTree {
			t.Fatalf("a working tree with an unreadable index answered %v (%v)", state, err)
		}
		tracked, err := repo.TracksUnder(".claude")
		if err == nil {
			t.Fatalf("an index that could not be read answered %v", tracked)
		}
		if len(tracked) != 0 {
			t.Errorf("a failed read came back with content: %v", tracked)
		}
	})

	// And the limit, measured: a repository whose .git camp cannot read is
	// reported by git itself as "not a git repository (or any parent up to
	// mount point /)" -- git's own no, character for character. camp reads
	// git's answer and has nothing else to read, so this arrives as an
	// ordinary "not a working tree" and the tracked-content rule then has
	// nothing to check. Whoever changes this has to change what is asked,
	// not how the answer is classified.
	t.Run("a .git git itself will not open", func(t *testing.T) {
		directory := build(t)
		dotgit := filepath.Join(directory, ".git")
		if err := os.Chmod(dotgit, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dotgit, 0o755) })

		if _, state, err := gitwire.Open(directory); state != gitwire.NotAWorkTree {
			t.Fatalf("an unreadable .git answered %v (%v), and git's own answer "+
				"is that this is not a repository", state, err)
		}
	})
}

// A checkout's path may hold a space, and git's line-oriented porcelain
// quotes such a path -- or, for a newline, splits one record across what
// reads as two lines. camp reads the NUL form, where a field is a field
// and the path is exactly the bytes on disk.
func TestAWorktreePathWithASpaceIsReadWhole(t *testing.T) {
	root := testenv.Root(t)
	t.Setenv("GIT_CEILING_DIRECTORIES", root)

	main := filepath.Join(root, "code")
	testenv.GitRepo(t, main)
	testenv.Write(t, filepath.Join(main, "README.md"), "the product\n")
	testenv.Commit(t, main, "the product")

	checkout := filepath.Join(root, "a checkout with spaces")
	command := exec.Command("git", "-C", main, "worktree", "add", "--quiet",
		"-b", "side", checkout)
	command.Env = append(os.Environ(), "LC_ALL=C",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("adding a worktree: %v\n%s", err, out)
	}

	worktrees, err := open(t, main).Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, worktree := range worktrees {
		if worktree.Path == checkout {
			found = true
			if worktree.Branch != "side" {
				t.Errorf("the branch came back as %q", worktree.Branch)
			}
		}
	}
	if !found {
		var paths []string
		for _, worktree := range worktrees {
			paths = append(paths, worktree.Path)
		}
		t.Errorf("the checkout at %q was not read back as itself: %q", checkout, paths)
	}
}
