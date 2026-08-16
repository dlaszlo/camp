package compose_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/mountx"
	"github.com/dlaszlo/camp/internal/nsx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/testenv"
)

// The composition, measured against the kernel rather than against the
// plan that produced it.
//
// Everything here runs inside a real user namespace, because that is the
// only way to find out whether a read-only bind is read-only. Creating
// one needs permission this machine grants by AppArmor profile to a
// single installed binary path, so a test binary built in the checkout
// cannot. These tests say so and skip; they pass when run through an
// installed camp.

const (
	insideEnv   = "CAMP_COMPOSE_INSIDE"
	configEnv   = "CAMP_COMPOSE_CONFIG"
	scenarioEnv = "CAMP_COMPOSE_SCENARIO"
)

func TestMain(m *testing.M) {
	if os.Getenv(insideEnv) != "" {
		os.Exit(inside())
	}
	os.Exit(m.Run())
}

// outcome is what the process inside the namespace reports back.
type outcome struct {
	Confined string `json:"confined"`

	Mounted  int      `json:"mounted"`
	Refused  []string `json:"refused"`
	Stranded []string `json:"stranded"`

	// What a process standing in the composed tree experiences.
	GuardedWrite  string `json:"guarded_write"`
	OverlayWrite  string `json:"overlay_write"`
	LandedInUpper bool   `json:"landed_in_upper"`
	RegistryWrite string `json:"registry_write"`
	LandedInRepo  bool   `json:"landed_in_repo"`
	WorkspaceOwn  string `json:"workspace_own"`
	ExcludeMatch  bool   `json:"exclude_match"`

	// Teardown.
	TornDown int      `json:"torn_down"`
	Stuck    []string `json:"stuck"`
	Residue  []string `json:"residue"`

	// The locked-flags scenario.
	OnTmpfs         bool   `json:"on_tmpfs"`
	LockedFlags     string `json:"locked_flags"`
	OmittedFlagsErr string `json:"omitted_flags_err"`

	Problems []string `json:"problems"`
}

func inside() int {
	out := outcome{}
	if profile, err := os.ReadFile("/proc/self/attr/apparmor/current"); err == nil {
		out.Confined = strings.TrimSpace(string(profile))
	}
	fail := func(format string, args ...any) {
		out.Problems = append(out.Problems, fmt.Sprintf(format, args...))
	}

	if err := nsx.Detach(); err != nil {
		fail("%v", err)
		return report(out)
	}
	if err := nsx.MountProc(); err != nil {
		fail("%v", err)
	}

	cfg, err := config.Load(os.Getenv(configEnv))
	if err != nil {
		fail("loading the configuration: %v", err)
		return report(out)
	}
	built, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Empty() {
		fail("validation: %s", refused.Error())
		return report(out)
	}
	generated, problems := gen.Derive(built)
	if !problems.Empty() {
		fail("generation: %s", problems.Error())
		return report(out)
	}

	setup := compose.Setup{
		Plan:    built,
		Prefix:  built.Live,
		Exclude: generated.Exclude,
		UID:     os.Getuid(),
		GID:     os.Getgid(),
	}

	switch os.Getenv(scenarioEnv) {
	case "locked-flags":
		return lockedFlags(out, built)
	case "shadow":
		return shadow(out, setup, built)
	}

	result := compose.Build(setup)
	out.Mounted = len(result.Mounted)
	out.Refused = result.Refused.Rules()
	out.Stranded = result.Stranded
	if !result.OK() {
		out.Problems = append(out.Problems, result.Refused.Error())
		return report(out)
	}

	probe(&out, built)
	teardown(&out, built, result)
	return report(out)
}

// probe asks what a process working in the composed tree would find.
func probe(out *outcome, built plan.Plan) {
	live := built.Live

	// A workspace-provided path has to fail loudly rather than copy up.
	out.GuardedWrite = writeResult(filepath.Join(live, "CLAUDE.md"), "edited\n")

	// A new file has to land in the code repository.
	out.OverlayWrite = writeResult(filepath.Join(live, "born.txt"), "x\n")
	if _, err := os.Stat(filepath.Join(built.Config.UpperPath(), "born.txt")); err == nil {
		out.LandedInUpper = true
	}

	// A writable bind's writes have to land in its own repository.
	out.RegistryWrite = writeResult(filepath.Join(live, ".registry", "note.txt"), "y\n")
	if _, err := os.Stat(filepath.Join(built.Config.Env, "registry", "note.txt")); err == nil {
		out.LandedInRepo = true
	}

	// And the workspace cannot be written through its own path either.
	out.WorkspaceOwn = writeResult(
		filepath.Join(built.Config.LowerPath(), "sneak.txt"), "z\n")

	if _, has := built.Config.GenerationStep(); has {
		payload, err := os.ReadFile(filepath.Join(live, ".git", "info", "exclude"))
		out.ExcludeMatch = err == nil && strings.Contains(string(payload), gen.Marker(built.Hash))
	}
}

func writeResult(path, content string) string {
	err := os.WriteFile(path, []byte(content), 0o644)
	switch {
	case err == nil:
		return "succeeded"
	case errors.Is(err, unix.EROFS):
		return "EROFS"
	case errors.Is(err, unix.EPERM), errors.Is(err, os.ErrPermission):
		return "EPERM"
	default:
		return err.Error()
	}
}

func teardown(out *outcome, built plan.Plan, result compose.Result) {
	setup := compose.Setup{Plan: built, Prefix: built.Live}
	targets := make([]string, 0, len(result.Mounted))
	for index := len(result.Mounted) - 1; index >= 0; index-- {
		targets = append(targets, setup.Target(result.Mounted[index]))
	}
	report := compose.Down(targets)
	out.TornDown = len(report.Removed)
	for _, stuck := range report.Stuck {
		out.Stuck = append(out.Stuck, stuck.Target)
	}
	if left, err := compose.Residue(built.Live); err == nil {
		out.Residue = left
	}
}

// shadow deliberately covers one of the composition's own mounts after
// it is up, and re-runs the verification.
//
// This is the check that has to catch it. A covered mount stays listed in
// the kernel's mount table -- it is still there, it is simply reachable
// by nothing -- so a verification that read the table would report
// everything present and correct. Only asking the path what it resolves
// to sees the difference.
func shadow(out outcome, setup compose.Setup, built plan.Plan) int {
	result := compose.Build(setup)
	if !result.OK() {
		out.Problems = append(out.Problems, result.Refused.Error())
		return report(out)
	}
	out.Mounted = len(result.Mounted)

	// Before shadowing, the pass has to be clean, or the test proves
	// nothing about what the shadow changed.
	if before := compose.Check(setup); !before.Empty() {
		out.Problems = append(out.Problems,
			"the composition did not verify before anything was shadowed: "+before.Error())
		return report(out)
	}

	target := filepath.Join(built.Live, ".registry")
	if err := unix.Mount(built.Config.LowerPath(), target, "", unix.MS_BIND, ""); err != nil {
		out.Problems = append(out.Problems, fmt.Sprintf("could not shadow: %v", err))
		return report(out)
	}
	out.Refused = compose.Check(setup).Rules()

	_, _ = mountx.Unmount(target)
	teardown(&out, built, result)
	return report(out)
}

// lockedFlags is the C34 scenario: a read-only remount inside a user
// namespace must replicate the source mount's locked flags or the kernel
// refuses it. The test asserts the fix, and reproduces the old bug only
// through the deliberately wrong call.
func lockedFlags(out outcome, built plan.Plan) int {
	source := built.Config.LowerPath()
	target := filepath.Join(built.Config.Env, "flagtest")
	if err := os.MkdirAll(target, 0o755); err != nil {
		out.Problems = append(out.Problems, err.Error())
		return report(out)
	}

	flags, err := mountx.LockedFlagsAt(source)
	if err != nil {
		out.Problems = append(out.Problems, err.Error())
		return report(out)
	}
	out.LockedFlags = mountx.DescribeFlags(flags)
	out.OnTmpfs = strings.Contains(out.LockedFlags, "nosuid")

	// The correct call.
	correct := plan.Mount{Kind: plan.BindRO, Source: source, Target: target}
	if err := mountx.Mount(correct); err != nil {
		out.Problems = append(out.Problems,
			fmt.Sprintf("the read-only bind with the locked flags replicated "+
				"failed: %v", err))
	} else {
		_, _ = mountx.Unmount(target)
	}

	// The deliberately wrong one.
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		out.Problems = append(out.Problems, err.Error())
		return report(out)
	}
	if err := mountx.RemountReadOnlyWithoutLockedFlags(target); err != nil {
		out.OmittedFlagsErr = err.Error()
	} else {
		out.OmittedFlagsErr = ""
	}
	_, _ = mountx.Unmount(target)
	return report(out)
}

func report(out outcome) int {
	encoded, _ := json.Marshal(out)
	os.Stdout.Write(encoded)
	return 0
}

// -- the parent side --------------------------------------------------------

func run(t *testing.T, env *testenv.Env, scenario string) outcome {
	t.Helper()

	cfg := env.Config(t, "")
	built, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Empty() {
		t.Fatalf("the fixture composition was refused before mounting:\n%v", refused)
	}
	if err := compose.Directories(built); err != nil {
		t.Fatalf("creating camp's own directories: %v", err)
	}
	if _, problems := gen.Prepare(built); !problems.Empty() {
		t.Fatalf("generation:\n%v", problems)
	}

	attrs, err := nsx.Own().Attrs()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	cmd.SysProcAttr = attrs
	cmd.Env = append(os.Environ(),
		insideEnv+"=1", configEnv+"="+cfg.Source, scenarioEnv+"="+scenario)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Skipf("the namespace child could not run, so this composition cannot "+
			"be measured from a checkout: %v\n%s", err, stderr.String())
	}

	var got outcome
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatalf("the child's report did not parse: %v\n%s", err, output)
	}
	if strings.HasPrefix(got.Confined, "unprivileged_userns") {
		t.Skipf("the namespace was created and AppArmor confined it to %q, "+
			"which denies every mount. Install camp and its profile to run this:\n"+
			"  sudo install -m 755 camp /usr/local/bin/camp\n"+
			"  sudo install -m 644 packaging/apparmor/camp /etc/apparmor.d/camp\n"+
			"  sudo apparmor_parser -r /etc/apparmor.d/camp", got.Confined)
	}
	return got
}

func TestTheCompositionPassesEveryCheckAndBehavesAsDesigned(t *testing.T) {
	env := testenv.NewEnv(t)
	got := run(t, env, "")

	for _, problem := range got.Problems {
		t.Errorf("inside the namespace: %s", problem)
	}
	if len(got.Refused) > 0 {
		t.Fatalf("the composition was refused after mounting: %v", got.Refused)
	}

	if got.GuardedWrite != "EROFS" {
		t.Errorf("writing a workspace-provided path returned %q, wanted EROFS. "+
			"That write has to fail loudly; the alternative is a silent copy-up "+
			"into the code repository", got.GuardedWrite)
	}
	if got.WorkspaceOwn != "EROFS" {
		t.Errorf("writing the workspace through its own absolute path returned "+
			"%q, wanted EROFS. The lower is never written, by any route",
			got.WorkspaceOwn)
	}
	if got.OverlayWrite != "succeeded" || !got.LandedInUpper {
		t.Errorf("a new file in the composed tree: write=%q, landed in the code "+
			"repository=%v", got.OverlayWrite, got.LandedInUpper)
	}
	if got.RegistryWrite != "succeeded" || !got.LandedInRepo {
		t.Errorf("a write to the record repository's mount: write=%q, landed in "+
			"the repository=%v", got.RegistryWrite, got.LandedInRepo)
	}
	if !got.ExcludeMatch {
		t.Error("the composed tree's .git/info/exclude does not carry camp's " +
			"generated block")
	}

	if len(got.Stuck) > 0 {
		t.Errorf("teardown left %v mounted", got.Stuck)
	}
	if len(got.Residue) > 0 {
		t.Errorf("the composed tree's directory is not empty after unmounting: "+
			"%v. That is evidence of something written where it should not have "+
			"been, and camp reports it rather than cleaning it away", got.Residue)
	}
}

// C34: the composition has to work on a nosuid filesystem, because the
// old failure there was a missing OR in the remount and not a property of
// tmpfs. The test asserts the fix; the bug is reproduced only by the
// deliberately flag-omitting call.
func TestACompositionOnTmpfsWorksWithTheLockedFlagsReplicated(t *testing.T) {
	t.Setenv("CAMP_TEST_ROOT", "/tmp")
	env := testenv.NewEnv(t)
	got := run(t, env, "locked-flags")

	for _, problem := range got.Problems {
		t.Errorf("inside the namespace: %s", problem)
	}
	if !got.OnTmpfs {
		t.Skipf("/tmp here carries the flags %q, so this machine cannot "+
			"exercise the nosuid case", got.LockedFlags)
	}
	t.Logf("the filesystem under /tmp has the locked flags %s", got.LockedFlags)
	if got.OmittedFlagsErr == "" {
		t.Error("the deliberately flag-omitting remount succeeded, so this " +
			"machine cannot tell the fix from the bug and the test above proves " +
			"nothing")
	}
}

func TestACompositionOnTmpfsPassesEndToEnd(t *testing.T) {
	t.Setenv("CAMP_TEST_ROOT", "/tmp")
	env := testenv.NewEnv(t)
	got := run(t, env, "")

	for _, problem := range got.Problems {
		t.Errorf("inside the namespace, on /tmp: %s", problem)
	}
	if len(got.Refused) > 0 {
		t.Errorf("the composition on /tmp was refused: %v", got.Refused)
	}
	if got.GuardedWrite != "EROFS" {
		t.Errorf("on /tmp, writing a workspace-provided path returned %q", got.GuardedWrite)
	}
}

func TestAShadowedMountIsCaught(t *testing.T) {
	env := testenv.NewEnv(t)
	got := run(t, env, "shadow")

	for _, problem := range got.Problems {
		t.Errorf("inside the namespace: %s", problem)
	}
	found := false
	for _, rule := range got.Refused {
		if rule == "verify-identity" {
			found = true
		}
	}
	if !found {
		t.Errorf("covering one of the composition's own mounts was not caught; "+
			"the rules that fired were %v.\nA covered mount stays in the kernel's "+
			"mount table, so this is exactly the case a table-based check would "+
			"miss and a path-based one must not", got.Refused)
	}
}

func TestANonEmptyLiveDirectoryStopsTheCompositionBeforeAnythingMounts(t *testing.T) {
	env := testenv.NewEnv(t)
	testenv.Write(t, filepath.Join(env.Live, "in-the-way.txt"), "x\n")

	cfg := env.Config(t, "")
	_, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Has("live-not-empty") {
		t.Fatalf("expected the composition to be refused for a non-empty live "+
			"directory; the rules that fired were %v", refused.Rules())
	}
}
