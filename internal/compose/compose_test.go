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
	"github.com/dlaszlo/camp/internal/enc"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/mountx"
	"github.com/dlaszlo/camp/internal/nsx"
	"github.com/dlaszlo/camp/internal/pathx"
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
	// The code repository's own path, frozen last: a write there, a write
	// through the .git bind cut from it before the freeze, and what
	// statvfs says at the three places the polarity has to differ.
	UpperRawWrite string `json:"upper_raw_write"`
	GitBindWrite  string `json:"git_bind_write"`
	UpperReadOnly bool   `json:"upper_read_only"`
	LiveReadOnly  bool   `json:"live_read_only"`
	GitReadOnly   bool   `json:"git_read_only"`
	ExcludeMatch  bool   `json:"exclude_match"`
	CodeSeesOwn   bool   `json:"code_sees_own"`
	IslandWrite   string `json:"island_write"`
	WaterWrite    string `json:"water_write"`

	// The repeated session.
	SecondRefused []string `json:"second_refused"`
	SecondMounted int      `json:"second_mounted"`

	// Teardown.
	TornDown int      `json:"torn_down"`
	Stuck    []string `json:"stuck"`
	Residue  []string `json:"residue"`

	// The sweep scenario: the lock lying about a running session.
	SweptWhileUp    []string `json:"swept_while_up"`
	KeptWhileUp     []string `json:"kept_while_up"`
	WriteAfterSweep string   `json:"write_after_sweep"`
	SweptAfterDown  []string `json:"swept_after_down"`

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
	built, refused := plan.Prepare(cfg)
	if !refused.Empty() {
		fail("validation: %s", refused.Error())
		return report(out)
	}
	generated, problems := gen.Adopt(built)
	if !problems.Empty() {
		fail("generation: %s", problems.Error())
		return report(out)
	}
	built = gen.Expand(built, generated)

	setup := compose.Setup{
		Plan:    built,
		Exclude: generated.Exclude,
		UID:     os.Getuid(),
		GID:     os.Getgid(),
	}

	switch os.Getenv(scenarioEnv) {
	case "locked-flags":
		return lockedFlags(out, built)
	case "shadow":
		return shadow(out, setup, built)
	case "repeat":
		return repeat(out, setup, built, cfg)
	case "sweep":
		return sweep(out, setup, built, cfg)
	case "freeze-before-git":
		return misordered(out, setup, plan.Declared)
	case "freeze-before-overlay":
		return misordered(out, setup, plan.Composed)
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

	// Nor the code repository through its own path: a write behind the
	// overlay's back would freeze what the tree shows at that path for the
	// rest of the session. The .git bind was cut from the writable mount
	// before the freeze, so writes through it still land.
	upper := built.Config.UpperPath()
	out.UpperRawWrite = writeResult(filepath.Join(upper, "behind-the-back.txt"), "w\n")
	out.GitBindWrite = writeResult(filepath.Join(live, ".git", "through-the-bind"), "g\n")
	os.Remove(filepath.Join(live, ".git", "through-the-bind"))
	out.UpperReadOnly = readOnly(upper)
	out.LiveReadOnly = readOnly(live)
	out.GitReadOnly = readOnly(filepath.Join(live, ".git"))

	// An island is read-only; the water around it takes writes and keeps
	// them, machine-local, past the end of the session.
	out.IslandWrite = writeResult(filepath.Join(live, ".claude", "settings.json"), "edited\n")
	out.WaterWrite = writeResult(filepath.Join(live, ".claude", "settings.local.json"), "{}\n")

	if _, has := built.Config.GenerationStep(); has {
		payload, err := os.ReadFile(filepath.Join(live, ".git", "info", "exclude"))
		out.ExcludeMatch = err == nil && strings.Contains(string(payload), gen.Marker(built.Hash))

		// The scoping that makes the whole approach honest: the bind is on
		// the composed tree's copy, so the code repository -- and any
		// checkout registered outside -- keeps reading its own file.
		own, err := os.ReadFile(filepath.Join(built.Config.UpperPath(), ".git", "info", "exclude"))
		out.CodeSeesOwn = err == nil && !strings.Contains(string(own), gen.Marker(built.Hash))
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

// readOnly asks the question the way a process writing there would
// experience the answer.
func readOnly(path string) bool {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return false
	}
	return fs.Flags&unix.ST_RDONLY != 0
}

// misordered moves the code repository's freeze to just before the first
// mount of the given role and builds that, so the test can assert what
// catches the wrong order.
//
// Both wrong orders are real: before the .git bind, the bind is cut from
// a read-only mount and inherits the flag; before the overlay, the kernel
// refuses to mount an overlay over a read-only upper at all. The frame
// puts the freeze last so that neither can happen; this measures that the
// verification (or the mount itself) would say so if it did.
func misordered(out outcome, setup compose.Setup, before plan.Role) int {
	var freeze plan.Mount
	var rest []plan.Mount
	for _, mount := range setup.Plan.Mounts {
		if mount.Role == plan.FreezeUpper {
			freeze = mount
			continue
		}
		rest = append(rest, mount)
	}
	var reordered []plan.Mount
	placed := false
	for _, mount := range rest {
		if !placed && mount.Role == before {
			reordered = append(reordered, freeze)
			placed = true
		}
		reordered = append(reordered, mount)
	}
	setup.Plan.Mounts = reordered

	result := compose.Build(setup)
	out.Mounted = len(result.Mounted)
	out.Refused = result.Refused.Rules()
	out.Stranded = result.Stranded
	if !result.Refused.Empty() {
		// Carried as a problem so the parent can read the text; the parent
		// decides whether refusing was right.
		out.Problems = append(out.Problems, result.Refused.Error())
	}
	if entries, err := os.ReadDir(setup.Plan.Live); err == nil {
		for _, entry := range entries {
			out.Residue = append(out.Residue, entry.Name())
		}
	}
	return report(out)
}

// teardown unmounts what the build made, in reverse, and looks at what is
// left in the composed tree's directory.
//
// camp itself never does this: the kernel discards the namespace and every
// mount in it when the session's last process exits. The test does it so
// that the property invariant 3 names can be measured -- the directory is
// empty once the mounts are gone, or something was written where it should
// not have been.
func teardown(out *outcome, built plan.Plan, result compose.Result) {
	for index := len(result.Mounted) - 1; index >= 0; index-- {
		target := result.Mounted[index].Target
		switch outcome, _ := mountx.Unmount(target); outcome {
		case mountx.Unmounted:
			out.TornDown++
		case mountx.Busy:
			out.Stuck = append(out.Stuck, target)
		}
	}
	if entries, err := os.ReadDir(built.Live); err == nil {
		for _, entry := range entries {
			out.Residue = append(out.Residue, entry.Name())
		}
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

// repeat is the second-run scenario: up, down, up again, with islands.
//
// The second run meets camp's own attachment points already standing in
// the water, and the collision rule -- which exists to stop camp hiding
// your machine-local files -- would refuse them if the scaffold manifest
// did not record whose they are. It also meets whatever the first session
// wrote into the water, which has to survive.
func repeat(out outcome, setup compose.Setup, built plan.Plan, cfg config.Config) int {
	first := compose.Build(setup)
	if !first.OK() {
		out.Problems = append(out.Problems, "first run: "+first.Refused.Error())
		return report(out)
	}
	out.Mounted = len(first.Mounted)

	// Something machine-local, of the kind that exists in no repository.
	kept := filepath.Join(built.Live, ".claude", "settings.local.json")
	out.WaterWrite = writeResult(kept, "{\"kept\":true}\n")
	teardown(&out, built, first)

	// And again, from the top, exactly as a second session would.
	second, refused := plan.Prepare(cfg)
	if !refused.Empty() {
		out.SecondRefused = refused.Rules()
		out.Problems = append(out.Problems, "second validation: "+refused.Error())
		return report(out)
	}
	generated, problems := gen.Prepare(second)
	if !problems.Empty() {
		out.SecondRefused = problems.Rules()
		out.Problems = append(out.Problems, "second generation: "+problems.Error())
		return report(out)
	}
	second = gen.Expand(second, generated)

	result := compose.Build(compose.Setup{
		Plan:    second,
		Exclude: generated.Exclude,
		UID:     os.Getuid(),
		GID:     os.Getgid(),
	})
	out.SecondRefused = result.Refused.Rules()
	out.SecondMounted = len(result.Mounted)
	if !result.OK() {
		out.Problems = append(out.Problems, "second run: "+result.Refused.Error())
		return report(out)
	}

	// The machine-local file is still there, through the composition.
	if _, err := os.Stat(kept); err != nil {
		out.Problems = append(out.Problems,
			"what the first session wrote into the water did not survive: "+err.Error())
	}
	probe(&out, second)
	teardown(&out, second, result)
	return report(out)
}

// sweep is the scenario the reported data loss came from: a sweep whose
// lock says the running session has ended.
//
// From inside a session the lock says nothing true about it -- its holder
// is outside this pid namespace and the live path is the overlay's own
// root, so /proc/locks is empty and a flock on the live path succeeds.
// The callback here answers what the lock answered there: every session
// has ended. The mount table has to overrule it while the overlay stands;
// and once the overlay is gone the same callback has to be believed
// again, because a work directory a session really did leave behind is
// what the sweep is for.
func sweep(out outcome, setup compose.Setup, built plan.Plan, cfg config.Config) int {
	result := compose.Build(setup)
	if !result.OK() {
		out.Problems = append(out.Problems, result.Refused.Error())
		return report(out)
	}
	out.Mounted = len(result.Mounted)
	ended := func(string) bool { return true }

	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		out.Problems = append(out.Problems, err.Error())
		return report(out)
	}
	out.SweptWhileUp, out.KeptWhileUp = compose.Sweep(cfg.Root, table, ended)
	// A write through the overlay needs its work directory, so this is the
	// session finding out whether it still has one.
	out.WriteAfterSweep = writeResult(filepath.Join(built.Live, "after-sweep.txt"), "x\n")

	teardown(&out, built, result)

	if table, err = mountinfo.Read(mountinfo.Self); err != nil {
		out.Problems = append(out.Problems, err.Error())
		return report(out)
	}
	out.SweptAfterDown, _ = compose.Sweep(cfg.Root, table, ended)
	return report(out)
}

// lockedFlags is the C41 scenario: a read-only remount inside a user
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
	if _, err := mountx.Mount(correct); err != nil {
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
	built, refused := plan.Prepare(cfg)
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

	// The code repository's own path is frozen, and the freeze reaches
	// neither the overlay nor the .git bind cut before it. Measured at the
	// three paths whose polarity has to differ, because that is the order
	// argument the frame makes and a check has to prove it rather than
	// assume it.
	if got.UpperRawWrite != "EROFS" || !got.UpperReadOnly {
		t.Errorf("writing the code repository through its own path returned %q "+
			"(statvfs read-only: %v), wanted EROFS. A write behind the overlay's "+
			"back freezes what the tree shows at that path for the rest of the "+
			"session, and inside a session the raw path has to refuse",
			got.UpperRawWrite, got.UpperReadOnly)
	}
	if got.LiveReadOnly {
		t.Error("the composed tree is read-only: the freeze of the code " +
			"repository reached the overlay, which means it was made before it")
	}
	if got.GitBindWrite != "succeeded" || got.GitReadOnly {
		t.Errorf("a write through the .git bind returned %q (statvfs read-only: "+
			"%v). A bind cut from a read-only mount inherits the flag, so the "+
			".git bind has to be made before the code repository is frozen",
			got.GitBindWrite, got.GitReadOnly)
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

// C41: the composition has to work on a nosuid filesystem, because the
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

// The scoping measurement: the exclude is bound on the composed tree's
// copy, so what git reports inside the composition changes and what it
// reports in the code repository does not.
func TestTheGeneratedExcludeIsVisibleOnlyThroughTheComposedTree(t *testing.T) {
	env := testenv.NewEnv(t)
	got := run(t, env, "")

	if !got.ExcludeMatch {
		t.Error("the composed tree's .git/info/exclude does not carry camp's block")
	}
	if !got.CodeSeesOwn {
		t.Error("the code repository's own .git/info/exclude carries camp's " +
			"block. It must not: the bind is on the live path, so the " +
			"repository and every checkout registered outside keep reading " +
			"their own file, and nothing camp does survives the session there")
	}
}

func TestAnIslandIsReadOnlyAndTheWaterAroundItIsNot(t *testing.T) {
	env := testenv.NewEnv(t)
	got := run(t, env, "")

	if got.IslandWrite != "EROFS" {
		t.Errorf("editing a contributed entry through the composed tree "+
			"returned %q, wanted EROFS. Islands fail loudly; a silent copy "+
			"anywhere is the failure this design exists to prevent", got.IslandWrite)
	}
	if got.WaterWrite != "succeeded" {
		t.Errorf("writing a machine-local file into the water returned %q. "+
			"That is what the water is for: a name that exists in no "+
			"repository has to have somewhere to live", got.WaterWrite)
	}
}

// up, down, up: the second run meets its own scaffolding and accepts it,
// and what the first session wrote into the water is still there.
func TestARepeatedSessionAcceptsItsOwnScaffolding(t *testing.T) {
	env := testenv.NewEnv(t)
	got := run(t, env, "repeat")

	for _, problem := range got.Problems {
		t.Errorf("inside the namespace: %s", problem)
	}
	if len(got.SecondRefused) > 0 {
		t.Errorf("the second run was refused: %v.\nThe attachment points camp "+
			"created in the water persist, so on the second up they are already "+
			"there -- and the collision rule would refuse camp's own objects "+
			"without the scaffold manifest to say whose they are",
			got.SecondRefused)
	}
	if got.SecondMounted != got.Mounted {
		t.Errorf("the first run made %d mounts and the second %d",
			got.Mounted, got.SecondMounted)
	}
}

func TestANonEmptyLiveDirectoryStopsTheCompositionBeforeAnythingMounts(t *testing.T) {
	env := testenv.NewEnv(t)
	testenv.Write(t, filepath.Join(env.Live, "in-the-way.txt"), "x\n")

	cfg := env.Config(t, "")
	_, refused := plan.Prepare(cfg)
	if !refused.Has("live-not-empty") {
		t.Fatalf("expected the composition to be refused for a non-empty live "+
			"directory; the rules that fired were %v", refused.Rules())
	}
}

// The reported data loss: 'camp shell' typed inside a running session
// swept that session's overlay work directory away, because from inside
// the lock said the session had ended. What the lock cannot see, the
// mount table can: an overlay naming a work directory is using it. And
// the same sweep, once the overlay is gone, still removes what an ended
// session left.
func TestAWorkDirectoryAnOverlayIsUsingIsNotSweptWhateverTheLockSays(t *testing.T) {
	env := testenv.NewEnv(t)
	got := run(t, env, "sweep")

	for _, problem := range got.Problems {
		t.Errorf("inside the namespace: %s", problem)
	}
	if len(got.SweptWhileUp) > 0 {
		t.Errorf("the sweep removed %v while an overlay was standing on it. The "+
			"lock lied, as it does from inside a session, and the mount table "+
			"was there to be asked", got.SweptWhileUp)
	}
	if len(got.KeptWhileUp) > 0 {
		t.Errorf("a work directory in use is not stale and has nothing to be "+
			"said about it, but the sweep reported: %v", got.KeptWhileUp)
	}
	if got.WriteAfterSweep != "succeeded" {
		t.Errorf("a write through the composed tree after the sweep returned "+
			"%q: the running session lost its work directory", got.WriteAfterSweep)
	}
	if len(got.SweptAfterDown) != 1 {
		t.Errorf("after the teardown the same sweep removed %v; the one work "+
			"directory the ended session left was what it is for",
			got.SweptAfterDown)
	}
}

// The sweep compares the kernel's spelling of a work directory with its
// own, and the kernel's spelling is not camp's: a space is written \040,
// a foreign overlay may have given a relative path or one through a link,
// and a container runtime's work directories sit under trees camp cannot
// read. Each of those has one honest answer, and none of them needs a
// namespace to measure.
func TestTheSweepReadsTheKernelsSpellingOfAWorkDirectory(t *testing.T) {
	base := filepath.Join(testenv.Root(t), "env with space")
	hash := "0123456789ab"
	entry := filepath.Join(base, config.Dir, "work", hash)
	own := filepath.Join(entry, "work")
	testenv.MkDir(t, own)
	marker := enc.Document([]string{
		enc.Line("live", filepath.Join(base, "gone")),
		enc.Line("config", filepath.Join(base, config.Dir, "config.yml")),
	})
	if err := os.WriteFile(filepath.Join(entry, compose.MarkerName), marker, 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := pathx.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ended := func(string) bool { return true }

	// Spelled as the kernel spells it: every space as \040.
	table := func(workdir string) []mountinfo.Entry {
		escape := func(path string) string { return strings.ReplaceAll(path, " ", `\040`) }
		line := "60 1 0:70 / " + escape(filepath.Join(base, "live")) +
			" rw,relatime - overlay none rw,lowerdir+=/l,upperdir=/u,workdir=" +
			escape(workdir) + ",userxattr"
		path := filepath.Join(testenv.Root(t), "mountinfo")
		if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		entries, err := mountinfo.Read(path)
		if err != nil {
			t.Fatal(err)
		}
		return entries
	}
	sweepWith := func(what, workdir string) (swept, kept []string) {
		t.Helper()
		swept, kept = compose.Sweep(root, table(workdir), ended)
		if _, err := os.Stat(own); err != nil && len(swept) == 0 {
			t.Fatalf("%s: the work directory is gone and the sweep did not say so", what)
		}
		return swept, kept
	}

	// The kernel's own spelling of this very directory, space and all.
	if swept, kept := sweepWith("escaped", own); len(swept)+len(kept) > 0 {
		t.Errorf("an overlay standing on this work directory, spelled the way the "+
			"kernel spells it, did not protect it: swept %v, kept %v", swept, kept)
	}
	// A spelling that cannot be compared: kept, and said to be.
	if swept, kept := sweepWith("relative", "work"); len(swept) > 0 || len(kept) != 1 ||
		!strings.Contains(kept[0], "for certain") {
		t.Errorf("an overlay with a relative work directory: swept %v, kept %v", swept, kept)
	}
	// A spelling the kernel's octal escaping never writes -- a backslash
	// kept verbatim by the legacy option parser -- cannot be decoded, so
	// camp cannot rule out that it is this directory: kept.
	if swept, kept := sweepWith("ambiguous", own+`\134x`); len(swept) > 0 || len(kept) != 1 ||
		!strings.Contains(kept[0], "with certainty") {
		t.Errorf("an overlay whose work directory camp cannot decode: swept %v, kept %v", swept, kept)
	}
	// A spelling through a symbolic link: the kernel followed it once, camp
	// does not follow it at all, and so cannot say where it went.
	link := filepath.Join(base, "alias")
	if err := os.Symlink(filepath.Join(base, config.Dir), link); err != nil {
		t.Fatal(err)
	}
	if swept, kept := sweepWith("symlink", filepath.Join(link, "work", hash, "work")); len(swept) > 0 || len(kept) != 1 {
		t.Errorf("an overlay whose work directory is spelled through a link: swept %v, kept %v", swept, kept)
	}
	// Somebody else's, under a tree camp cannot read: not camp's own.
	private := filepath.Join(base, "private")
	testenv.MkDir(t, private)
	if err := os.Chmod(private, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(private, 0o755) })
	if swept, kept := sweepWith("unreadable", filepath.Join(private, "snapshots", "7475", "work")); len(swept) != 1 || len(kept) > 0 {
		t.Errorf("a container runtime's overlay stopped the sweep: swept %v, kept %v", swept, kept)
	}
}

// The two wrong orders for the code repository's freeze, each caught.
//
// Before the .git bind: the bind is cut from a read-only mount and
// inherits the flag, and the polarity check names it under
// verify-read-only. Before the overlay: the kernel refuses to mount an
// overlay over a read-only upper at all, so it is the mount that fails,
// not a later check. Either way nothing is left mounted and the composed
// tree's directory is bare.
func TestTheFreezeOfTheCodeRepositoryHasToComeLast(t *testing.T) {
	t.Run("before the .git bind", func(t *testing.T) {
		got := run(t, testenv.NewEnv(t), "freeze-before-git")
		if !contains(got.Refused, "verify-read-only") {
			t.Errorf("the freeze made before the .git bind was refused as %v, "+
				"wanted verify-read-only: the bind cut from the frozen mount is "+
				"read-only, and the polarity check is what has to say so.\n%s",
				got.Refused, strings.Join(got.Problems, "\n"))
		}
		if !strings.Contains(strings.Join(got.Problems, "\n"), "/.git") {
			t.Errorf("the refusal does not name the .git bind:\n%s",
				strings.Join(got.Problems, "\n"))
		}
		if len(got.Stranded) > 0 || len(got.Residue) > 0 {
			t.Errorf("the refused composition left something behind: stranded %v, "+
				"residue %v", got.Stranded, got.Residue)
		}
	})
	t.Run("before the overlay", func(t *testing.T) {
		got := run(t, testenv.NewEnv(t), "freeze-before-overlay")
		if !contains(got.Refused, "mount-failed") {
			t.Errorf("the freeze made before the overlay was refused as %v, wanted "+
				"mount-failed: the kernel does not mount an overlay over a "+
				"read-only upper.\n%s", got.Refused, strings.Join(got.Problems, "\n"))
		}
		if len(got.Stranded) > 0 || len(got.Residue) > 0 {
			t.Errorf("the refused composition left something behind: stranded %v, "+
				"residue %v", got.Stranded, got.Residue)
		}
	})
}

func contains(rules []string, rule string) bool {
	for _, have := range rules {
		if have == rule {
			return true
		}
	}
	return false
}
