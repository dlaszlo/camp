package plan_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/testenv"
)

func prepare(t *testing.T, env *testenv.Env, yaml string) (plan.Plan, refusal.List) {
	t.Helper()
	return plan.Prepare(env.Config(t, yaml))
}

func mustPass(t *testing.T, env *testenv.Env, yaml string) plan.Plan {
	t.Helper()
	built, refused := prepare(t, env, yaml)
	if !refused.Empty() {
		t.Fatalf("this composition should have been accepted; it was refused:\n\n%v", refused)
	}
	return built
}

func mustRefuse(t *testing.T, env *testenv.Env, yaml, rule string) refusal.List {
	t.Helper()
	_, refused := prepare(t, env, yaml)
	if refused.Has(rule) {
		return refused
	}
	t.Fatalf("expected the rule %q to fire; the rules that did were %v\n\n%v",
		rule, refused.Rules(), refused.Error())
	return nil
}

// The steady state: the whole target configuration passes, and derives
// the frame the specification describes.
func TestTargetCompositionIsAccepted(t *testing.T) {
	env := testenv.NewEnv(t)
	built := mustPass(t, env, "")

	if built.Mounts[0].Role != plan.FreezeLower {
		t.Errorf("the first mount is %s; the workspace's self-bind has to come "+
			"first, or there is a window in which the lower is writable",
			built.Mounts[0].Role)
	}
	if built.Mounts[1].Role != plan.Composed {
		t.Errorf("the second mount is %s, wanted the overlay", built.Mounts[1].Role)
	}
	last := built.Mounts[len(built.Mounts)-1]
	if last.Role != plan.FreezeUpper || last.Kind != plan.BindRO || last.InLive ||
		last.Source != env.Code || last.Target != env.Code {
		t.Errorf("the last mount is %s %s %s -> %s (in the tree: %v); the code "+
			"repository's read-only self-bind has to come last: an overlay does "+
			"not mount over a read-only upper, and a bind cut from a read-only "+
			"mount inherits the flag, so every step sourcing from the repository "+
			"has to exist before its path is frozen",
			last.Role, last.Kind, last.Source, last.Target, last.InLive)
	}

	guards := map[string]bool{}
	for _, mount := range built.Mounts {
		if mount.Role == plan.RootGuard {
			guards[mount.Rel.String()] = true
		}
	}
	// .git, .claude and .registry are mount targets; .gitignore is
	// allow-listed. Everything else the workspace has at its root is
	// protected, and nothing is named in the configuration to make that
	// happen.
	for _, name := range []string{"CLAUDE.md", "AGENTS.md", ".workspace"} {
		if !guards[name] {
			t.Errorf("%q got no read-only protection; a write to it would copy "+
				"up into the code repository", name)
		}
	}
	for _, name := range []string{".git", ".claude", ".registry", ".gitignore"} {
		if guards[name] {
			t.Errorf("%q got a derived protection although a mount already "+
				"covers it (or allow_overlap names it)", name)
		}
	}

	if len(built.IslandsMounts) != 1 {
		t.Fatalf("%d islands mounts were derived, wanted 1", len(built.IslandsMounts))
	}
	if built.IslandsMounts[0].Target.String() != ".claude" {
		t.Errorf("the islands mount targets %q", built.IslandsMounts[0].Target)
	}
}

func TestHashIsDerivedFromTheLivePathAndIsStable(t *testing.T) {
	env := testenv.NewEnv(t)
	first := mustPass(t, env, "").Hash
	second := mustPass(t, env, "").Hash
	if first != second {
		t.Errorf("the same composition hashed to %s and then %s; work/ and "+
			"storage/ are named from this, so an orphan could not be attributed",
			first, second)
	}
	if len(first) != 12 {
		t.Errorf("the hash is %d characters, wanted 12", len(first))
	}
}

// The order in steps: is the mount order, and a total order is only worth
// having if it is checked. Child before parent is refused, because the
// parent would silently cover the child; parent before child is the
// everyday case and passes.
func TestChildBeforeParentIsRefusedAndTheReverseOrderPasses(t *testing.T) {
	env := testenv.NewEnv(t)

	// git_exclude targets .git/info/exclude, which lies inside the .git
	// mount's target. Listed first, the .git bind would cover it.
	wrong := `env: ` + env.Path + `
merged: live
repositories:
  - { name: workspace, path: workspace }
  - { name: code,      path: code }
overlayfs: { lower: [workspace], upper: code }
allow_overlap: [.gitignore, .registry, .claude]
steps:
  - git_exclude
  - mount_rw:
      - { source: "code/.git", target: ".git" }
`
	list := mustRefuse(t, env, wrong, "target-nested")
	if !strings.Contains(list.Error(), "Parent first, then child") {
		t.Error("the refusal should say which way round to put them")
	}

	// The shipped order -- .git first, its exclude afterwards -- is the
	// legal one and has to keep passing.
	mustPass(t, env, "")
}

func TestTwoMountsOnOneTargetAreRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	yaml := strings.Replace(env.YAML(),
		`      - { source: "registry",  target: ".registry" }`,
		`      - { source: "registry",  target: ".registry" }
      - { source: "registry",  target: ".registry" }`, 1)
	mustRefuse(t, env, yaml, "target-duplicate")
}

// The .git bind is not derived and camp never adds it: leaving it out is
// caught by the overlap gate, by a rule that knows nothing about git.
func TestOmittingTheGitMountIsStoppedByTheGate(t *testing.T) {
	env := testenv.NewEnv(t)
	yaml := strings.Replace(env.YAML(),
		`      - { source: "code/.git", target: ".git" }
`, "", 1)

	list := mustRefuse(t, env, yaml, "overlap")
	if !strings.Contains(list.Error(), ".git") {
		t.Error("the refusal should name .git")
	}
	for _, rule := range list.Rules() {
		if strings.Contains(rule, "git-") || rule == "git-missing" {
			t.Errorf("the omission was caught by a git-specific rule (%q); it "+
				"has to be caught structurally, so that a composition of two "+
				"non-git directories never mentions the name", rule)
		}
	}
}

func TestAnOverlapStopsTheComposition(t *testing.T) {
	env := testenv.NewEnv(t)
	testenv.Write(t, filepath.Join(env.Workspace, "README.md"), "workspace readme\n")

	list := mustRefuse(t, env, "", "overlap")
	message := list.Error()
	for _, want := range []string{
		"README.md",
		filepath.Join(env.Workspace, "README.md"),
		filepath.Join(env.Code, "README.md"),
		"unreachable",
		"allow_overlap",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal does not mention %q; it has to name the path, "+
				"both sides, which side wins and both ways out:\n\n%s", want, message)
		}
	}
}

// Inside an allow-listed directory the check keeps going one level down:
// a file on both sides of a merged directory is what a copy-up leaves
// behind, and that is the thing most worth catching.
func TestTheGateDescendsIntoAnAllowListedDirectory(t *testing.T) {
	env := testenv.NewEnv(t)
	testenv.Write(t, filepath.Join(env.Workspace, "shared", "notes.md"), "workspace\n")
	testenv.Write(t, filepath.Join(env.Code, "shared", "notes.md"), "code\n")
	testenv.Write(t, filepath.Join(env.Code, "shared", "only-code.md"), "code\n")

	yaml := strings.Replace(env.YAML(), "allow_overlap: [.gitignore]",
		"allow_overlap: [.gitignore, shared]", 1)
	list := mustRefuse(t, env, yaml, "overlap")
	if !strings.Contains(list.Error(), filepath.Join("shared", "notes.md")) {
		t.Errorf("the file present on both sides was not reported:\n\n%v", list.Error())
	}
	if strings.Contains(list.Error(), "only-code.md") {
		t.Error("a file only one side has is not an overlap and must not be reported")
	}
}

func TestANonEmptyLiveDirectoryIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	testenv.Write(t, filepath.Join(env.Live, "left-behind.txt"), "x\n")

	list := mustRefuse(t, env, "", "live-not-empty")
	if !strings.Contains(list.Error(), "left-behind.txt") {
		t.Error("the refusal should name what is in the way")
	}
	if !strings.Contains(list.Error(), "camp status") {
		t.Error("the refusal should point at status, in case something is " +
			"mounted there rather than written there")
	}
}

// A composed tree's directory that is not there stops nothing: a session
// creates it. git cannot record an empty directory, so no clone of an
// environment can bring one, and refusing would meet every fresh checkout
// with a repair for the one thing camp can safely make itself.
func TestAMissingLiveDirectoryIsAWarningAndNotARefusal(t *testing.T) {
	env := testenv.NewEnv(t)
	if err := os.Remove(env.Live); err != nil {
		t.Fatal(err)
	}
	cfg := env.Config(t, "")
	built, refused := plan.Prepare(cfg)
	if !refused.Empty() {
		t.Fatalf("a missing composed tree refused the composition:\n%v", refused)
	}
	if built.Live != env.Live {
		t.Fatalf("the plan was derived for %q", built.Live)
	}
	var said bool
	for _, warning := range built.Warnings {
		if strings.Contains(warning, env.Live) {
			said = true
		}
	}
	if !said {
		t.Errorf("nothing says the directory is not there yet: %v", built.Warnings)
	}
}

// A parent that is not there is a different thing: it is a typo in
// merged:, and building a path of directories to reach one would put the
// composition somewhere nobody meant.
func TestALiveDirectoryWithNoParentIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	if err := os.Remove(env.Live); err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(env.YAML(), "merged: live", "merged: deeper/inside/live", 1)
	list := mustRefuse(t, env, yaml, "live-parent-missing")
	if !strings.Contains(list.Error(), "merged:") {
		t.Error("the refusal should name the setting that is wrong")
	}
}

func TestASymlinkedLiveDirectoryIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	if err := os.Remove(env.Live); err != nil {
		t.Fatal(err)
	}
	testenv.MkDir(t, filepath.Join(env.Path, "elsewhere"))
	testenv.Symlink(t, filepath.Join(env.Path, "elsewhere"), env.Live)
	mustRefuse(t, env, "", "live-symlink")
}

func TestASymlinkedSourceIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	if err := os.RemoveAll(env.Registry); err != nil {
		t.Fatal(err)
	}
	testenv.MkDir(t, filepath.Join(env.Path, "real-registry"))
	testenv.Symlink(t, filepath.Join(env.Path, "real-registry"), env.Registry)

	list := mustRefuse(t, env, "", "repository-symlink")
	if !strings.Contains(list.Error(), "repointed") {
		t.Error("the refusal should say why a link is refused rather than followed")
	}
}

func TestASymlinkAtTheWorkspaceRootIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	testenv.Symlink(t, "/etc", filepath.Join(env.Workspace, "outside"))
	mustRefuse(t, env, "", "root-entry-symlink")
}

// The lower is never written, by any route. A writable mount sourced from
// it would be writable in name and refused by the kernel in practice, in
// the middle of a session.
func TestAWritableMountFromTheLowerIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	yaml := strings.Replace(env.YAML(),
		`      - { source: "registry",  target: ".registry" }`,
		`      - { source: "workspace/.workspace", target: ".registry" }`, 1)
	list := mustRefuse(t, env, yaml, "source-under-lower")
	if !strings.Contains(list.Error(), "repository of its own") {
		t.Error("the refusal should say what the way out is")
	}
}

// Covering tracked code makes git report those files deleted, and
// "git commit -a" records it.
func TestAMountOverTrackedCodeIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	testenv.Write(t, filepath.Join(env.Workspace, "src", ".gitkeep"), "")
	testenv.Commit(t, env.Workspace, "a mount point that collides with code")

	yaml := strings.Replace(env.YAML(),
		`      - { source: "registry",  target: ".registry" }`,
		`      - { source: "registry",  target: "src" }`, 1)
	list := mustRefuse(t, env, yaml, "target-tracked-code")
	if !strings.Contains(list.Error(), "src/app.go") {
		t.Error("the refusal should name the tracked file that would vanish")
	}
}

// .git and .git/info/exclude pass the tracked-content rule automatically,
// because git tracks nothing under .git. The rule needs no exception list
// and must not grow one.
func TestTheGitMountsNeedNoExceptionFromTheTrackedRule(t *testing.T) {
	env := testenv.NewEnv(t)
	built := mustPass(t, env, "")

	var sawGit, sawExclude bool
	for _, mount := range built.Mounts {
		switch mount.Rel.String() {
		case ".git":
			sawGit = true
		case ".git/info/exclude":
			sawExclude = true
		}
	}
	if !sawGit || !sawExclude {
		t.Errorf("the plan is missing a git mount: .git=%v exclude=%v", sawGit, sawExclude)
	}
}

func TestAMissingMountPointIsRefusedWithTheRepair(t *testing.T) {
	env := testenv.NewEnv(t)
	if err := os.RemoveAll(filepath.Join(env.Workspace, ".registry")); err != nil {
		t.Fatal(err)
	}
	list := mustRefuse(t, env, "", "target-missing")
	if !strings.Contains(list.Error(), "placeholder") {
		t.Error("the refusal should say how a mount point is committed -- git " +
			"cannot track an empty directory")
	}
}

func TestAMissingExcludeFileIsRefusedWithBothCommands(t *testing.T) {
	env := testenv.NewEnv(t)
	if err := os.RemoveAll(filepath.Join(env.Code, ".git", "info")); err != nil {
		t.Fatal(err)
	}
	list := mustRefuse(t, env, "", "target-missing")
	message := list.Error()
	if !strings.Contains(message, "mkdir -p") || !strings.Contains(message, "touch") {
		t.Errorf("the refusal should print the two commands the user runs:\n\n%s", message)
	}
}

func TestATypeMismatchIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	// A file target where the source is a directory.
	yaml := strings.Replace(env.YAML(),
		`      - { source: "registry",  target: ".registry" }`,
		`      - { source: "registry",  target: "CLAUDE.md" }`, 1)
	mustRefuse(t, env, yaml, "target-type")
}

func TestTwoRepositoriesResolvingToOneDirectoryAreRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	testenv.Symlink(t, env.Registry, filepath.Join(env.Path, "registry-alias"))
	yaml := strings.Replace(env.YAML(),
		`  - { name: registry,  path: registry }`,
		`  - { name: registry,  path: registry }
  - { name: alias,     path: registry-alias }`, 1)
	// The alias is a symlink, so it is refused for that first -- which is
	// the stronger reason. Compare by inode with a real second path.
	mustRefuse(t, env, yaml, "repository-symlink")

	hard := strings.Replace(env.YAML(),
		`  - { name: registry,  path: registry }`,
		`  - { name: registry,  path: registry }
  - { name: alias,     path: ./registry }`, 1)
	if _, err := env.TryConfig(hard); err == nil {
		t.Error("a path with a '.' component should be refused by the grammar")
	}
}

func TestANestedRepositoryIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	testenv.GitRepo(t, filepath.Join(env.Code, "vendor", "inner"))
	yaml := strings.Replace(env.YAML(),
		`  - { name: registry,  path: registry }`,
		`  - { name: registry,  path: registry }
  - { name: inner,     path: code/vendor/inner }`, 1)
	mustRefuse(t, env, yaml, "repository-nested")
}

func TestALiveDirectoryInsideARepositoryIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	testenv.MkDir(t, filepath.Join(env.Code, "live"))
	yaml := strings.Replace(env.YAML(), "merged: live", "merged: code/live", 1)
	mustRefuse(t, env, yaml, "live-in-repository")
}

func TestAMissingRepositoryIsRefusedWithoutOfferingToCloneIt(t *testing.T) {
	env := testenv.NewEnv(t)
	if err := os.RemoveAll(env.Registry); err != nil {
		t.Fatal(err)
	}
	list := mustRefuse(t, env, "", "repository-missing")
	if !strings.Contains(list.Error(), "camp neither clones nor creates") {
		t.Error("the refusal should say camp will not do it for you")
	}
}

// A composition without a generation step has no exclude at all. That is
// legal, and the plan says so plainly rather than leaving the defence out
// silently.
func TestAConfigurationWithoutAGenerationStepIsLegal(t *testing.T) {
	env := testenv.NewEnv(t)
	yaml := strings.Replace(env.YAML(), "  - git_exclude\n", "", 1)
	built := mustPass(t, env, yaml)
	if _, has := built.Config.GenerationStep(); has {
		t.Error("no generation step should have been found")
	}
	for _, mount := range built.Mounts {
		if mount.Role == plan.Artefact {
			t.Error("an exclude was planned although no step generates one")
		}
	}
}

// The overlay asks for userxattr by name. The kernel forces it inside a
// user namespace anyway, and the plan still has to say so, because a plan
// that does not say what it means cannot be checked against what happened.
func TestTheOverlayAsksForUserXattr(t *testing.T) {
	built := mustPass(t, testenv.NewEnv(t), "")
	for _, mount := range built.Mounts {
		if mount.Kind == plan.Overlay {
			if mount.Xattr != "userxattr" {
				t.Errorf("the composed tree asks for %q", mount.Xattr)
			}
			return
		}
	}
	t.Fatal("the fixture plans no composed tree")
}

func TestWorkAndStorageDoNotShareAParent(t *testing.T) {
	env := testenv.NewEnv(t)
	built := mustPass(t, env, "")
	if filepath.Dir(built.Work) == filepath.Dir(built.Storage) {
		t.Errorf("work (%s) and storage (%s) share a parent; their lifecycles "+
			"are opposite and one of them must never be swept with the other",
			built.Work, built.Storage)
	}
	if !strings.HasPrefix(built.Storage, filepath.Join(env.Path, config.Dir)) {
		t.Errorf("storage landed outside the environment's camp directory: %s", built.Storage)
	}
}

// A composition whose git cannot be read does not start.
//
// The rule that no mount may cover tracked content is the one thing camp
// asks git during planning. Reading a failed git as "tracks nothing" made
// that rule pass without running, which is how a mount that hides tracked
// files -- and a 'git commit -a' that records them as deleted -- would
// have been accepted.
func TestGitFailingDuringPlanningStopsTheComposition(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")

	// Everything the fixture needed git for is done. From here there is no
	// git to ask, and the code repository is unchanged.
	t.Setenv("PATH", filepath.Join(env.Path, "nothing-here"))

	_, refused := plan.Prepare(cfg)
	if !refused.Has("git-unreadable") {
		t.Fatalf("the composition was not refused for an unanswerable git; it "+
			"answered %v", refused.Rules())
	}
}

// git answering one question and failing the next is still git failing.
//
// The frame question is answered from .git and needs no index, so a
// repository whose index cannot be read opens as an ordinary working
// tree -- and the question the rule turns on, what it tracks under a
// mount point, fails. Read as "tracks nothing", that failure is the
// tracked-content rule not running, on the one path it exists to guard:
// a mount over tracked content makes git report those files deleted and
// 'git commit -a' record the deletion.
func TestATrackedContentAnswerThatFailsStopsTheComposition(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")

	index := filepath.Join(env.Code, ".git", "index")
	if err := os.Chmod(index, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(index, 0o644) })

	_, refused := plan.Prepare(cfg)
	if !refused.Has("git-unreadable") {
		t.Fatalf("a code repository that could not answer what it tracks was "+
			"accepted; the rules that fired were %v", refused.Rules())
	}
	if !strings.Contains(refused.Error(), "index file open failed") {
		t.Errorf("the refusal does not carry git's own reason:\n%s", refused.Error())
	}
}

// allow_overlap exempts a name from the overlap gate. It does not make
// the name expressible, and it does not make a socket or a symlink
// something camp can stand over.
//
// It does not even require an overlap: a name can be listed there and
// exist only in the workspace. Such a name used to be exempt from
// everything -- no read-only bind, no exclude line, and no type check --
// which is protected by nothing at all.
func TestAnAllowListedNameIsStillJudgedForWhatItIs(t *testing.T) {
	env := testenv.NewEnv(t)
	link := filepath.Join(env.Workspace, "outside")
	if err := os.Symlink(filepath.Join(env.Path, "elsewhere"), link); err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(env.YAML(), "allow_overlap: [.gitignore]",
		"allow_overlap: [.gitignore, outside]", 1)

	cfg := env.Config(t, yaml)
	env.Accept(t, cfg)
	_, refused := plan.Prepare(cfg)
	if !refused.Has("root-entry-symlink") {
		t.Fatalf("an allow-listed symlink at the workspace root was accepted; "+
			"the rules that fired were %v", refused.Rules())
	}
}

// A directory the descent cannot read is a check that did not run, which
// is not a check that found nothing. Inside an allow-listed directory
// that check is the one that catches a copy-up.
func TestAnUnreadableAllowListedDirectoryStopsTheComposition(t *testing.T) {
	env := testenv.NewEnv(t)
	for _, side := range []string{env.Workspace, env.Code} {
		testenv.Write(t, filepath.Join(side, "shared", "note.md"), "note\n")
	}
	if err := os.Chmod(filepath.Join(env.Workspace, "shared"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(env.Workspace, "shared"), 0o755) })

	yaml := strings.Replace(env.YAML(), "allow_overlap: [.gitignore]",
		"allow_overlap: [.gitignore, shared]", 1)
	cfg := env.Config(t, yaml)
	env.Accept(t, cfg)
	_, refused := plan.Prepare(cfg)
	if !refused.Has("overlap-unreadable") {
		t.Fatalf("an unreadable allow-listed directory passed the gate; the "+
			"rules that fired were %v", refused.Rules())
	}
}
