package drift_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/drift"
	"github.com/dlaszlo/camp/internal/inventory"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/testenv"
)

func built(t *testing.T, env *testenv.Env) plan.Plan {
	t.Helper()
	cfg := env.Config(t, "")
	p, refused := plan.Prepare(cfg)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}
	return p
}

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, repo, err, out)
	}
	return string(out)
}

// The scan that sees a forced add.
//
// `git add -f` through the composed tree reads a workspace file and
// stages its bytes, leaving an *indexed* path with no file in the raw
// working tree at all -- so a scan for untracked files is structurally
// blind to it. The leak is constructed here the same way the measurement
// was: with update-index --cacheinfo, which produces exactly that state
// without needing a composition to be mounted.
func TestTheIndexScanCatchesAForcedAdd(t *testing.T) {
	env := testenv.NewEnv(t)
	p := built(t, env)

	blob := strings.TrimSpace(gitInput(t, env.Code, "instructions\n", "hash-object", "-w", "--stdin"))
	git(t, env.Code, "update-index", "--add", "--cacheinfo", "100644,"+blob+",CLAUDE.md")

	// The proof that an untracked scan cannot see this.
	others := git(t, env.Code, "--no-optional-locks", "ls-files", "--others", "--exclude-standard")
	if strings.Contains(others, "CLAUDE.md") {
		t.Fatal("the constructed leak left a working-tree file, so it is not the " +
			"case this scan exists for")
	}

	found := drift.Scan(p)
	var seen bool
	for _, path := range found.Indexed {
		if path == "CLAUDE.md" {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("the index scan did not find the staged workspace file; it "+
			"found %v", found.Indexed)
	}
	if !strings.Contains(found.String(), "restore --staged") {
		t.Error("the report should give the command that undoes it")
	}
	if !strings.Contains(found.String(), "push, not commit") {
		t.Error("the report should say why this is still worth catching")
	}
}

func gitInput(t *testing.T, repo, input string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = append(os.Environ(), "LC_ALL=C",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, repo, err)
	}
	return string(out)
}

// A file in the code repository whose name belongs to the workspace is
// suspected copy-up residue: a write that should have been refused and
// landed there instead.
func TestTheUntrackedScanReportsSuspectedResidue(t *testing.T) {
	env := testenv.NewEnv(t)
	p := built(t, env)

	testenv.Write(t, filepath.Join(env.Code, ".workspace", "docs", "leaked.md"), "oops\n")

	found := drift.Scan(p)
	var seen bool
	for _, path := range found.Untracked {
		if strings.HasPrefix(path, ".workspace/") {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("the untracked scan found %v", found.Untracked)
	}
	if !strings.Contains(found.String(), "copy-up residue") {
		t.Error("the report should say what it thinks it is looking at")
	}
}

// A name the code repository legitimately has at its own root is a code
// path, and finding it there proves nothing.
func TestACodePathIsNotReportedAsALeak(t *testing.T) {
	env := testenv.NewEnv(t)
	p := built(t, env)

	testenv.Write(t, filepath.Join(env.Code, "src", "new.go"), "package main\n")

	found := drift.Scan(p)
	for _, path := range found.Untracked {
		if strings.HasPrefix(path, "src/") {
			t.Errorf("an ordinary new file in the code repository was reported "+
				"as a leak: %s", path)
		}
	}
}

// A worktree registered inside the composed tree dies with it, and the
// report has to give the exact command that makes it independent -- with
// a path that outlives the composition.
func TestAWorktreeInsideTheTreeGetsARepairCommand(t *testing.T) {
	env := testenv.NewEnv(t)
	p := built(t, env)

	// A worktree registered at a path inside the composed tree, the way one
	// created through the tree would be.
	inside := filepath.Join(env.Live, ".claude", "worktrees", "lane1")
	git(t, env.Code, "worktree", "add", "--detach", "--quiet", inside)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(env.Live, ".claude")) })

	found := drift.Scan(p)
	if len(found.Worktrees) == 0 {
		t.Fatal("the worktree registered inside the composed tree was not found")
	}

	worktree := found.Worktrees[0]
	if worktree.Repair == "" {
		t.Fatal("no repair command was produced")
	}
	if strings.Contains(worktree.Backing, env.Live) {
		t.Errorf("the repair names %s, which is inside the composition and dies "+
			"with it. It has to name a path that outlives the composition",
			worktree.Backing)
	}
	if !strings.Contains(worktree.Backing, p.Storage) {
		t.Errorf("the backing path came out as %s; a worktree under an islands "+
			"mount really lives in camp's storage", worktree.Backing)
	}
	if !strings.Contains(found.String(), "gc.worktreePruneExpire") {
		t.Error("the report should name the window in which git prunes the " +
			"registration, because that is the failure that happens while " +
			"nobody is looking")
	}
}

// Backing maps a path in the tree to where its content really is: the
// deepest mount that covers it decides.
func TestBackingResolvesThroughTheDeepestMount(t *testing.T) {
	env := testenv.NewEnv(t)
	p := built(t, env)

	cases := []struct {
		path string
		want string
	}{
		{filepath.Join(env.Live, "src", "app.go"), filepath.Join(env.Code, "src", "app.go")},
		{filepath.Join(env.Live, "CLAUDE.md"), filepath.Join(env.Workspace, "CLAUDE.md")},
		{filepath.Join(env.Live, ".registry", "events.jsonl"), filepath.Join(env.Registry, "events.jsonl")},
		{filepath.Join(env.Live, ".claude", "worktrees"), filepath.Join(p.Storage, ".claude", "worktrees")},
	}
	for _, test := range cases {
		got, found := drift.Backing(p, test.path)
		if !found || got != test.want {
			t.Errorf("%s resolves to %q (found=%v), wanted %q",
				test.path, got, found, test.want)
		}
	}
}

// The gate's comparison re-runs at the end of a session, so an overlap
// that appeared during it is named the same day.
func TestTheGateComparisonRerunsAndReports(t *testing.T) {
	env := testenv.NewEnv(t)
	testenv.Write(t, filepath.Join(env.Workspace, "README.md"), "appeared\n")

	cfg := env.Config(t, "")
	p, _ := plan.Prepare(cfg)
	if p.Live == "" {
		t.Skip("the composition was refused before a plan existed")
	}

	found := drift.Refresh(p)
	if len(found.Overlaps) == 0 {
		t.Fatal("an overlap that appeared was not reported")
	}
	if !strings.Contains(found.String(), "README.md") {
		t.Error("the report should name it")
	}
}

func TestNothingToSayMeansNothingIsSaid(t *testing.T) {
	env := testenv.NewEnv(t)
	found := drift.Scan(built(t, env))
	if !found.Empty() {
		t.Errorf("a clean composition produced a report:\n%s", found.String())
	}
	if found.String() != "" {
		t.Error("an empty report should render as nothing at all")
	}
}

// A scan that could not run says so. An omitted scan reads exactly like a
// scan that found nothing, and these run at the end of a session -- the
// one moment when the cause of a mid-session change is still fresh.
func TestAScanThatCouldNotRunIsSaidRatherThanOmitted(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	built, refused := plan.Prepare(cfg)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}

	// The accepted snapshot is damaged. The comparison against it is the
	// one that names a workspace root entry born during the session.
	if err := os.WriteFile(inventory.Path(cfg.Env), []byte("not a record\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := drift.Refresh(built)
	if len(report.Failures) == 0 {
		t.Fatalf("a damaged snapshot was reported as nothing to say:\n%s", report.String())
	}
	if !strings.Contains(report.String(), "did not run") {
		t.Errorf("the report does not say the comparison did not run:\n%s", report.String())
	}

	// And a root that cannot be read is not answered from the listing camp
	// started with, which is exactly the listing this pass exists to
	// question.
	if err := os.Chmod(cfg.LowerPath(), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(cfg.LowerPath(), 0o755) })
	report = drift.Refresh(built)
	if len(report.Failures) == 0 {
		t.Fatalf("an unreadable workspace root was reported as nothing to "+
			"say:\n%s", report.String())
	}
	if !strings.Contains(report.String(), cfg.LowerPath()) {
		t.Errorf("the report does not name the root it could not read:\n%s",
			report.String())
	}
}
