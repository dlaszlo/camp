// Package testenv builds real directory trees for the tests.
//
// Everything camp decides before it mounts can be decided from ordinary
// directories, so the tests construct real trees and never need root.
// What cannot be tested that way -- that the kernel does what the mount
// asks -- is not pretended to be tested here.
//
// The scratch root deliberately does not use t.TempDir(). That lands in
// /tmp, which is commonly a tmpfs mounted nosuid,nodev, and the tests
// that go on to mount something need a filesystem whose locked flags a
// namespace can replicate. One root for every test keeps the surprise out
// of the tests that mount, at no cost to the ones that do not.
// CAMP_TEST_ROOT overrides it.
package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/inventory"
	"github.com/dlaszlo/camp/internal/pathx"
)

// RepoRoot returns the module's own root, for the tests that read camp's
// source as data -- the guards that keep a rule true by failing the build.
// OwnModule reports whether a path belongs to this module rather than to
// one nested inside its tree.
//
// The source guards walk the repository and hold every .go file in it to
// a rule about camp -- that every write goes through fsx, that no unmount
// is lazy, that every fsconfig call goes through the description. A
// directory with a go.mod of its own is not camp: measure/ is the
// instruments, a separate module that imports nothing of camp's on
// purpose, and it reads and writes with the standard library because that
// is what measuring camp from outside means.
//
// So the guards ask this. It is the same test RepoRoot uses to find the
// root, applied downwards: the nearest go.mod above a file is the module
// it is in.
func OwnModule(t *testing.T, root, path string) bool {
	t.Helper()
	directory := filepath.Dir(path)
	for {
		if directory == root {
			return true
		}
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return false
		}
		parent := filepath.Dir(directory)
		if parent == directory || len(parent) < len(root) {
			return true
		}
		directory = parent
	}
}

func RepoRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("no go.mod above the test's directory")
		}
		directory = parent
	}
}

// Tracked returns the absolute path of every file this repository tracks.
//
// The guards that keep a rule true by failing the build walk this rather
// than the directory tree beneath the module root. Run inside a camp
// composition, that root also carries whatever the composition mounts
// there -- another repository's documents among them, which are free to
// quote the very strings the guards forbid -- and a guard whose answer
// depends on where it was run from is not a guard.
//
// It asks git rather than guessing at which directories are camp's own,
// because "what this repository contains" is a question git answers
// exactly. A file that has not been added yet is not in the repository
// yet, and is not scanned.
func Tracked(t *testing.T) []string {
	t.Helper()
	root := RepoRoot(t)

	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("listing what %s tracks: %v", root, err)
	}

	var paths []string
	for _, name := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if name != "" {
			paths = append(paths, filepath.Join(root, name))
		}
	}
	if len(paths) == 0 {
		t.Fatalf("%s tracks no files, so a guard walking them would pass "+
			"without looking at anything", root)
	}
	return paths
}

// SkipIfItCouldMount skips a test that is about what the mount
// primitives do when the kernel refuses them.
//
// An ordinary user can ask a mount syscall exactly one question and get a
// real answer: what happens when it is not allowed. open_tree with
// OPEN_TREE_CLONE needs CAP_SYS_ADMIN, so unprivileged it fails, nothing
// is attached, and the shape of the refusal -- the error, and the claim
// about what is standing afterwards -- is a contract a test can hold to.
// A process that does hold the capability would get the other answer: it
// would make a mount. The tests that ask this question are not written to
// clean one up, and this repository's tests must not leave one behind, so
// they skip instead.
func SkipIfItCouldMount(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("this is running as root, and this test must not make a mount")
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("this test needs to know whether it could mount, and "+
			"/proc/self/status could not be read: %v", err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		rest, found := strings.CutPrefix(line, "CapEff:")
		if !found {
			continue
		}
		effective, err := strconv.ParseUint(strings.TrimSpace(rest), 16, 64)
		if err != nil {
			t.Skipf("this test needs to know whether it could mount, and "+
				"CapEff reads %q: %v", strings.TrimSpace(rest), err)
		}
		// CAP_SYS_ADMIN is bit 21, and it is the one that would turn the
		// refusal these tests measure into a mount.
		if effective&(1<<21) != 0 {
			t.Skip("this process holds CAP_SYS_ADMIN, and this test must not " +
				"make a mount")
		}
		return
	}
	t.Skip("/proc/self/status names no effective capability set, so whether " +
		"this could mount is unknown")
}

// Root returns a scratch directory that is removed when the test ends.
func Root(t *testing.T) string {
	t.Helper()

	base := os.Getenv("CAMP_TEST_ROOT")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("finding a home directory for the scratch root: %v", err)
		}
		base = filepath.Join(home, "overlayfs", ".camp-tests")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("creating the scratch root %s: %v", base, err)
	}
	dir, err := os.MkdirTemp(base, "t-")
	if err != nil {
		t.Fatalf("creating a scratch directory under %s: %v", base, err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// Env is a scratch environment directory in the shape camp expects.
type Env struct {
	// Path is the environment root -- what env: names.
	Path string
	// Workspace, Code and Registry are absolute repository paths.
	Workspace string
	Code      string
	Registry  string
	// Live is the empty directory the composed tree would appear in.
	Live string
}

// NewEnv builds the layout the specification is written against: a code
// repository, a workspace repository owning the development environment,
// a separate record repository, and an empty directory for the composed
// tree.
//
// Zero overlap between the two roots except .gitignore, which each
// repository needs its own copy of -- the steady state the migration is
// meant to establish.
func NewEnv(t *testing.T) *Env {
	t.Helper()
	root := Root(t)

	env := &Env{
		Path:      root,
		Workspace: filepath.Join(root, "workspace"),
		Code:      filepath.Join(root, "code"),
		Registry:  filepath.Join(root, "registry"),
		Live:      filepath.Join(root, "live"),
	}

	GitRepo(t, env.Code)
	Write(t, filepath.Join(env.Code, "src", "app.go"), "package main\n")
	Write(t, filepath.Join(env.Code, "README.md"), "the product\n")
	Write(t, filepath.Join(env.Code, ".gitignore"), "/node_modules\n")

	GitRepo(t, env.Workspace)
	Write(t, filepath.Join(env.Workspace, "CLAUDE.md"), "instructions\n")
	Write(t, filepath.Join(env.Workspace, "AGENTS.md"), "agents\n")
	Write(t, filepath.Join(env.Workspace, ".gitignore"), "/.claude/worktrees\n")
	Write(t, filepath.Join(env.Workspace, ".claude", "settings.json"), "{}\n")
	Write(t, filepath.Join(env.Workspace, ".claude", "agents", "reviewer.md"), "reviewer\n")
	Write(t, filepath.Join(env.Workspace, ".workspace", "docs", "topology.md"), "docs\n")
	// The record repository's mount point: committed and empty, because a
	// bind cannot create its own mount point and git cannot track an empty
	// directory.
	Write(t, filepath.Join(env.Workspace, ".registry", ".gitkeep"), "")

	GitRepo(t, env.Registry)
	Write(t, filepath.Join(env.Registry, "events.jsonl"), "")

	// Committed, because the checks that read git have to meet tracked
	// content and not an empty index.
	Commit(t, env.Code, "the product")
	Commit(t, env.Workspace, "the development environment")
	Commit(t, env.Registry, "the record")

	MkDir(t, env.Live)
	return env
}

// YAML returns a configuration for this environment: the target shape of
// the specification's own example, with the paths of this scratch tree.
func (e *Env) YAML() string {
	return `env: ` + e.Path + `
merged: live

repositories:
  - { name: workspace, path: workspace }
  - { name: code,      path: code }
  - { name: registry,  path: registry }

overlayfs:
  lower: [workspace]
  upper: code

allow_overlap: [.gitignore]

steps:
  - mount_rw:
      - { source: "code/.git", target: ".git" }
      - { source: "registry",  target: ".registry" }
  - mount_islands:
      - { source: "workspace/.claude", target: ".claude" }
  - git_exclude
`
}

// Config writes a configuration into the environment and parses it,
// failing the test if it does not parse. Pass an empty string for the
// default shape.
//
// Written to disk, not only parsed in memory: the tests that mount
// something run in a second process, which finds the composition the way
// every camp command does -- by reading the file.
func (e *Env) Config(t *testing.T, yaml string) config.Config {
	t.Helper()
	if yaml == "" {
		yaml = e.YAML()
	}
	path := config.Path(e.Path)
	Write(t, path, yaml)

	cfg, err := config.Parse([]byte(yaml), path)
	if err != nil {
		t.Fatalf("the fixture configuration did not parse:\n%v", err)
	}
	e.Accept(t, cfg)
	return cfg
}

// Accept writes the snapshot of both roots that camp compares against at
// every up, the way 'camp accept' would.
//
// Every fixture starts from an accepted state, because that is the steady
// state camp is judged in. The tests that are about the snapshot change
// something after this and watch what camp says.
func (e *Env) Accept(t *testing.T, cfg config.Config) {
	t.Helper()
	lower, err := pathx.ReadDirBeneath(cfg.LowerPath(), nil)
	if err != nil {
		return // the fixture is deliberately broken; the test is about that
	}
	upper, err := pathx.ReadDirBeneath(cfg.UpperPath(), nil)
	if err != nil {
		return
	}
	if err := inventory.Take(lower, upper).Save(cfg.Root); err != nil {
		t.Fatalf("writing the fixture inventory: %v", err)
	}
}

// TryConfig parses a configuration and returns whatever came back, for
// the tests that are about the refusal.
func (e *Env) TryConfig(yaml string) (config.Config, error) {
	return config.Parse([]byte(yaml), config.Path(e.Path))
}

// GitRepo makes a real git repository, because the checks that read git
// have to be tested against git and not against a directory that looks
// like one.
func GitRepo(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	git(t, path, "init", "--quiet", "-b", "main")
	git(t, path, "config", "user.name", "camp tests")
	git(t, path, "config", "user.email", "tests@example.invalid")
	git(t, path, "config", "commit.gpgsign", "false")
	return path
}

// Commit stages everything in a repository and commits it, so that the
// tracked-content checks have tracked content to find.
func Commit(t *testing.T, path, message string) {
	t.Helper()
	git(t, path, "add", "-A")
	git(t, path, "commit", "--quiet", "--allow-empty", "-m", message)
}

func git(t *testing.T, path string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", path}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, path, err, out)
	}
}

// Write creates a file and every directory above it.
func Write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// MkDir creates a directory and every directory above it.
func MkDir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	return path
}

// Symlink creates a symbolic link, for the tests about refusing them.
func Symlink(t *testing.T, target, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("linking %s -> %s: %v", path, target, err)
	}
	return path
}
