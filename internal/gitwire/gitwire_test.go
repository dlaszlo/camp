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

// noRepositoryAbove establishes what every test about "this directory is
// in no repository" needs: that nothing above the scratch root is a
// working tree.
//
// It used to be arranged with GIT_CEILING_DIRECTORIES in camp's own
// environment. camp passes no ambient variable to git any more -- that is
// the whole of this file's other subject -- so the fact has to be true of
// the machine instead of arranged in an environment. It is measured
// rather than assumed, and a machine whose scratch root really does sit
// inside a checkout cannot construct it at all, so the test says that
// instead of failing for a reason that is not about camp.
func noRepositoryAbove(t *testing.T, root string) {
	t.Helper()
	command := exec.Command("git", "-C", root, "rev-parse", "--is-inside-work-tree")
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LC_ALL=C"}
	if out, err := command.Output(); err == nil && strings.TrimSpace(string(out)) == "true" {
		t.Skipf("the scratch root %s is itself inside a git working tree, so a "+
			"directory in no repository cannot be built under it. Point "+
			"CAMP_TEST_ROOT somewhere that is not.", root)
	}
}

// A directory in no repository at all is not an error and not a git
// repository either -- it is the answer that sends the caller to the raw
// listing, which is a usable answer and camp says so where it is noticed.
func TestADirectoryInNoRepositoryIsNotOpened(t *testing.T) {
	root := testenv.Root(t)
	noRepositoryAbove(t, root)

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
	noRepositoryAbove(t, root)

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
		noRepositoryAbove(t, root)
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

	// And the case that used to be the limit. A repository whose .git camp
	// cannot read is reported by git itself as "not a git repository (or
	// any parent up to mount point /)" -- git's own no, character for
	// character, measured. camp used to read git's answer and have nothing
	// else to read, so a damaged repository arrived as an ordinary "not a
	// working tree" and the tracked-content rule had nothing to check.
	// There is one more thing to read, and camp reads it: whether the
	// directory it asked about carries a .git at all.
	t.Run("a .git git itself will not open", func(t *testing.T) {
		directory := build(t)
		dotgit := filepath.Join(directory, ".git")
		if err := os.Chmod(dotgit, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dotgit, 0o755) })

		repo, state, err := gitwire.Open(directory)
		if state != gitwire.Unreadable {
			t.Fatalf("a .git camp cannot read answered %v (%v); there is a "+
				"repository there and git could not read it, which is the "+
				"opposite of no repository", state, err)
		}
		if repo != nil {
			t.Error("a handle came back for a repository git could not read")
		}
		if err == nil || !strings.Contains(err.Error(), ".git") {
			t.Errorf("the reason does not name what was found there: %v", err)
		}
	})

	// The shape a real broken checkout has: .git is a control file naming
	// a gitdir, and the gitdir is gone -- a worktree whose main repository
	// was moved or deleted. Measured: git answers "fatal: not a git
	// repository: <the missing path>", exit 128, which is the same
	// sentence it uses for a directory that is in no repository at all.
	t.Run("a .git control file whose gitdir is gone", func(t *testing.T) {
		directory := build(t)
		dotgit := filepath.Join(directory, ".git")
		if err := os.RemoveAll(dotgit); err != nil {
			t.Fatal(err)
		}
		testenv.Write(t, dotgit,
			"gitdir: "+filepath.Join(filepath.Dir(directory), "no-such-gitdir")+"\n")

		repo, state, err := gitwire.Open(directory)
		if state != gitwire.Unreadable {
			t.Fatalf("a .git pointing at a gitdir that is not there answered "+
				"%v (%v); read as no repository, the rule that no mount may "+
				"cover tracked content simply does not run", state, err)
		}
		if repo != nil {
			t.Error("a handle came back for a repository git could not read")
		}
	})
}

// somebodyElsesRepository is a second real repository: the one a
// redirected read lands in. It tracks a file this fixture does not have,
// so a query that goes there answers visibly wrong instead of plausibly
// right.
func somebodyElsesRepository(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(testenv.Root(t), "elsewhere")
	testenv.GitRepo(t, directory)
	testenv.Write(t, filepath.Join(directory, "other.txt"), "another repository\n")
	testenv.Commit(t, directory, "somebody else's repository")
	return directory
}

// Nothing in the environment camp was started with may change which
// repository git is asked about, or what it answers.
//
// The repository is named by -C and by nothing else, and every variable
// below takes that name back out of the environment. Each is set the way
// a terminal, a wrapper script or a parent process would export it -- in
// camp's own environment, not in the test's git commands -- and the two
// questions camp asks have to come back unchanged.
//
// What each one did before the environment was built rather than
// inherited, measured on this fixture:
//
//   - GIT_DIR at another repository: every answer is that repository's.
//     At a path that does not exist, exit 128 and "fatal: not a git
//     repository" -- git's own no, so Open said NotAWorkTree and the
//     tracked-content rule was skipped rather than refused.
//   - GIT_WORK_TREE elsewhere: --is-inside-work-tree prints false and
//     exits 0. An ordinary no, with no failure anywhere to notice.
//   - GIT_INDEX_FILE at a path that does not exist: still a working tree,
//     and ls-files prints nothing and exits 0. An empty tracked set is
//     how camp reads "this mount covers nothing tracked".
//   - GIT_COMMON_DIR and GIT_OBJECT_DIRECTORY: git stops discovering from
//     -C at all and reports no repository.
//   - GIT_CEILING_DIRECTORIES at the repository root: discovery cannot
//     reach it from a subdirectory, and a workspace that sits inside a
//     larger repository is no longer in one.
//   - GIT_CONFIG_COUNT with core.excludesFile: every untracked file
//     disappears from the leak scan, which is the scan that exists to
//     find what a session copied into the code repository.
func TestAmbientGitVariablesCannotChangeWhatGitIsAskedAbout(t *testing.T) {
	for shape, build := range shapes(t) {
		t.Run(shape, func(t *testing.T) {
			directory := build(t)
			other := somebodyElsesRepository(t)
			ignoreEverything := testenv.Write(t,
				filepath.Join(filepath.Dir(other), "ignore-everything"), "*\n")
			globalConfig := testenv.Write(t,
				filepath.Join(filepath.Dir(other), "global-config"),
				"[core]\n\texcludesFile = "+ignoreEverything+"\n")

			hostile := []struct {
				name string
				set  map[string]string
			}{
				{"GIT_DIR at another repository",
					map[string]string{"GIT_DIR": filepath.Join(other, ".git")}},
				{"GIT_DIR at nothing",
					map[string]string{"GIT_DIR": filepath.Join(other, "no-such-git-dir")}},
				{"GIT_WORK_TREE", map[string]string{"GIT_WORK_TREE": other}},
				{"GIT_INDEX_FILE",
					map[string]string{"GIT_INDEX_FILE": filepath.Join(other, "no-such-index")}},
				{"GIT_COMMON_DIR",
					map[string]string{"GIT_COMMON_DIR": filepath.Join(other, "no-such-common-dir")}},
				{"GIT_OBJECT_DIRECTORY",
					map[string]string{"GIT_OBJECT_DIRECTORY": filepath.Join(other, "no-such-objects")}},
				{"GIT_CEILING_DIRECTORIES",
					map[string]string{"GIT_CEILING_DIRECTORIES": filepath.Dir(directory)}},
				{"GIT_CONFIG_GLOBAL",
					map[string]string{"GIT_CONFIG_GLOBAL": globalConfig}},
				{"GIT_CONFIG_COUNT", map[string]string{
					"GIT_CONFIG_COUNT":   "1",
					"GIT_CONFIG_KEY_0":   "core.excludesFile",
					"GIT_CONFIG_VALUE_0": ignoreEverything,
				}},
			}

			for _, hostile := range hostile {
				t.Run(hostile.name, func(t *testing.T) {
					for name, value := range hostile.set {
						t.Setenv(name, value)
					}

					repo := open(t, directory)
					tracked, err := repo.TracksUnder(".claude")
					if err != nil {
						t.Fatalf("asking what is tracked: %v", err)
					}
					mustEqual(t, "what is tracked under .claude", tracked,
						[]string{".claude/agents/reviewer.md", ".claude/settings.json"})

					untracked, err := repo.Untracked()
					if err != nil {
						t.Fatalf("asking what is untracked: %v", err)
					}
					mustEqual(t, "the untracked paths", untracked, []string{"stray.txt"})
				})
			}
		})
	}
}

// A checkout's path may hold a space, and git's line-oriented porcelain
// quotes such a path -- or, for a newline, splits one record across what
// reads as two lines. camp reads the NUL form, where a field is a field
// and the path is exactly the bytes on disk.
func TestAWorktreePathWithASpaceIsReadWhole(t *testing.T) {
	root := testenv.Root(t)
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
