package session_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/locks"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/session"
	"github.com/dlaszlo/camp/internal/testenv"
)

// The supervisor's behaviour, measured rather than described.
//
// Every test here starts a real session, which needs permission to create
// a user namespace. This machine grants that by AppArmor profile to one
// installed binary path, so these skip from a checkout and pass when run
// through an installed camp.

// The test binary is the one that gets re-executed as the namespace's
// init, so it has to answer to the same hidden argument the real binary
// does. Without this the child would simply run the test suite again.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == session.InitArg {
		session.InitMain(os.Args[2:])
		return
	}
	os.Exit(m.Run())
}

// ready is a composition prepared exactly as the launcher prepares one:
// validated, generated, expanded, with both locks held.
type ready struct {
	Env     *testenv.Env
	Config  config.Config
	Plan    plan.Plan
	Exclude []byte
	Locks   *locks.Pair
}

// prepare does the launcher's first four steps. Every test here needs
// all of them, and a test that skipped one would be measuring a session
// camp would never start.
func prepare(t *testing.T) *ready {
	t.Helper()
	return prepareWith(t, testenv.NewEnv(t), "")
}

func prepareWith(t *testing.T, env *testenv.Env, yaml string) *ready {
	t.Helper()

	cfg := env.Config(t, yaml)
	built, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}
	if err := compose.Directories(built); err != nil {
		t.Fatal(err)
	}
	generated, problems := gen.Prepare(built)
	if !problems.Empty() {
		t.Fatalf("generation was refused:\n%v", problems)
	}

	pair, err := takeLocks(cfg)
	if err != nil {
		t.Fatalf("the locks could not be taken: %v", err)
	}
	t.Cleanup(pair.Release)

	return &ready{
		Env:     env,
		Config:  cfg,
		Plan:    gen.Expand(built, generated),
		Exclude: generated.Exclude,
		Locks:   pair,
	}
}

// start runs the session and returns the workload's exit status.
func (r *ready) start(argv []string, stdin, stdout, stderr *os.File) (int, error) {
	return session.Launch(session.Options{
		Config:  r.Config,
		Plan:    r.Plan,
		Exclude: r.Exclude,
		Argv:    argv,
		Locks:   r.Locks,
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
	})
}

// devnull is the stdio a test hands a workload that has nothing to say.
func devnull(t *testing.T) *os.File {
	t.Helper()
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file
}

func takeLocks(cfg config.Config) (*locks.Pair, error) {
	repo, ok := cfg.Repository(cfg.Upper)
	if !ok {
		return nil, errors.New("the fixture names no upper")
	}
	return locks.TakePair(cfg.Env, repo.Path.Components(), cfg.Merged.Components(),
		cfg.UpperPath(), cfg.Live())
}

func skipUnlessNamespaced(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	var single refusal.R
	if errors.As(err, &single) && single.Rule == "namespace-denied" {
		t.Skipf("this binary may not create a user namespace, so a session " +
			"cannot be started from a checkout. Install camp and its profile:\n" +
			"  sudo install -m 755 camp /usr/local/bin/camp\n" +
			"  sudo install -m 644 packaging/apparmor/camp /etc/apparmor.d/camp\n" +
			"  sudo apparmor_parser -r /etc/apparmor.d/camp")
	}
	if strings.Contains(err.Error(), "unprivileged_userns") ||
		strings.Contains(err.Error(), "operation not permitted") ||
		strings.Contains(err.Error(), "permission denied") {
		t.Skipf("the namespace could not be built here: %v", err)
	}
}

// -- what the workload is handed -------------------------------------------
//
// Everything below is about construction rather than about a running
// session, so it needs no namespace: what a declared PATH selects, what
// the child's environment ends up being, and that building it changed
// nothing about the process that built it.

// workloadEnv resolves an environment and selects a command from it, the
// way the init does either side of the capability drop.
func workloadEnv(t *testing.T, tail string, base []string, argv ...string) session.Workload {
	t.Helper()
	env := testenv.NewEnv(t)
	yaml := ""
	if tail != "" {
		yaml = env.YAML() + tail
	}
	environment, err := session.Resolve(env.Config(t, yaml), env.Live, base)
	if err != nil {
		t.Fatalf("the environment could not be resolved: %v", err)
	}
	built, err := environment.Workload(env.Live, argv)
	if err != nil {
		t.Fatalf("the workload could not be constructed: %v", err)
	}
	return built
}

// The lookup rule the declared PATH exists for: a bare command name is
// found through the session's own PATH, not through camp's. Resolving
// against camp's path while the plan prints the declared one would run the
// host's command under a plan that says otherwise -- and the
// workspace-owned launcher directory a composition prepends would never be
// reached.
func TestABareCommandIsSelectedByTheDeclaredPath(t *testing.T) {
	directory := filepath.Join(testenv.Root(t), "bin")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(directory, "camp-test-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	declaration := "\nsession:\n  environment:\n    PATH: \"" + directory + ":$PATH\"\n"
	base := []string{"PATH=/nowhere"}

	built := workloadEnv(t, declaration, base, "camp-test-tool")
	if built.Path != tool {
		t.Errorf("the command resolved to %q, wanted %q -- the declared PATH is "+
			"what the workload will have, so it is what has to select the command",
			built.Path, tool)
	}

	// And an argv that names a file is not searched for at all.
	built = workloadEnv(t, declaration, base, "/bin/sh", "-c", "true")
	if built.Path != "/bin/sh" {
		t.Errorf("an absolute command resolved to %q", built.Path)
	}
}

// A command the session's PATH does not carry fails loudly, says which
// PATH was searched, and prints none of it.
func TestAMissingCommandNamesThePathWithoutPrintingIt(t *testing.T) {
	env := testenv.NewEnv(t)
	secret := "/opt/private/tools"
	cfg := env.Config(t, env.YAML()+
		"\nsession:\n  environment:\n    PATH: \""+secret+"\"\n")

	environment, err := session.Resolve(cfg, env.Live, []string{"PATH=/usr/bin"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = environment.Workload(env.Live, []string{"camp-test-absent"})
	if err == nil {
		t.Fatal("a command that is nowhere on the declared PATH was accepted")
	}
	if !strings.Contains(err.Error(), "PATH this session applies") {
		t.Errorf("the failure does not say which PATH was searched:\n%v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the failure printed the PATH it searched:\n%v", err)
	}
}

// The effective environment: camp's two names win, nothing appears twice,
// and everything the map does not mention arrives byte for byte.
func TestTheWorkloadsEnvironmentIsCampsWholeContract(t *testing.T) {
	base := []string{
		"HOME=/home/someone",
		"PATH=/usr/bin",
		"CAMP_LIVE=/an/older/session",
		"PWD=/where/camp/was/run",
	}
	built := workloadEnv(t, "\nsession:\n  environment:\n    TOOL: \"$HOME/x\"\n",
		base, "/bin/sh")

	found := map[string]string{}
	for _, entry := range built.Env {
		name, value, _ := strings.Cut(entry, "=")
		if _, twice := found[name]; twice {
			t.Errorf("%q appears twice in the workload's environment", name)
		}
		found[name] = value
	}
	for name, want := range map[string]string{
		"CAMP_LIVE": built.Dir,
		"PWD":       built.Dir,
		"HOME":      "/home/someone",
		"TOOL":      "/home/someone/x",
	} {
		if found[name] != want {
			t.Errorf("%s is %q, wanted %q", name, found[name], want)
		}
	}
	if _, present := found["CAMP_SESSION"]; present {
		t.Error("CAMP_SESSION is in the workload's environment. It is not part of " +
			"the contract: an exported session marker invites host-side wrappers " +
			"that switch on it, which is wiring the host through another door")
	}

}

// The names a session applies are reported, and only the names.
func TestTheAppliedNamesAreReportedWithoutTheirValues(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, env.YAML()+`
session:
  environment:
    ZULU: "z"
    ALPHA: "$HOME"
`)
	environment, err := session.Resolve(cfg, env.Live, []string{"HOME=/home/someone"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(environment.Applied, ",") != "ALPHA,ZULU" {
		t.Errorf("the applied names came out as %v, wanted them in byte order",
			environment.Applied)
	}
}

// The init's own environment is never touched. A declared PATH installed
// on camp's own process would be a configured value steering a process
// that still holds CAP_SYS_ADMIN.
func TestBuildingAWorkloadDoesNotTouchThisProcess(t *testing.T) {
	before := strings.Join(os.Environ(), "\n")
	workloadEnv(t, "\nsession:\n  environment:\n    TOOL: \"x\"\n",
		[]string{"PATH=/usr/bin"}, "/bin/sh")
	if after := strings.Join(os.Environ(), "\n"); after != before {
		t.Error("constructing a workload changed the environment of the process " +
			"that constructed it")
	}
}

// An empty argv means a shell, and the shell is the effective one: a
// session that declares SHELL has said which shell it means.
func TestTheShellComesFromTheEffectiveEnvironment(t *testing.T) {
	built := workloadEnv(t, "\nsession:\n  environment:\n    SHELL: \"/bin/sh\"\n",
		[]string{"SHELL=/bin/definitely-not-here", "PATH=/usr/bin"})
	if built.Path != "/bin/sh" {
		t.Errorf("the shell came out as %q, wanted the declared one", built.Path)
	}
}

// The name is gone from the tool, not merely unused by it. Anything built
// on an exported session marker is built on a convention somebody else may
// already be using for something of their own.
func TestCampSessionAppearsNowhereInTheSource(t *testing.T) {
	root := testenv.RepoRoot(t)
	self := filepath.Join(root, "internal", "session", "session_test.go")

	var offenders []string
	for _, path := range testenv.Tracked(t) {
		if path == self {
			continue // this test names it in order to forbid it
		}
		switch filepath.Ext(path) {
		case ".go", ".md", ".yml", ".yaml":
		default:
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "CAMP_SESSION") {
			relative, _ := filepath.Rel(root, path)
			offenders = append(offenders, relative)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("CAMP_SESSION still appears in %v", offenders)
	}
}

// The launcher waits for the workload, not for the init, and exits with
// the workload's own status.
func TestAForegroundCommandsExitStatusIsPropagated(t *testing.T) {
	quiet := devnull(t)
	status, err := prepare(t).start(
		[]string{"/bin/sh", "-c", "exit 7"}, quiet, quiet, os.Stderr)
	skipUnlessNamespaced(t, err)
	if err != nil {
		t.Fatalf("the session failed: %v", err)
	}
	if status != 7 {
		t.Errorf("the session exited %d and the command exited 7. The status is "+
			"the command's own, and not camp's to reinterpret", status)
	}
}

// A command that dies on a signal reports 128 plus the signal, the way a
// shell does.
func TestASignalledWorkloadReportsTheShellsConvention(t *testing.T) {
	quiet := devnull(t)
	status, err := prepare(t).start(
		[]string{"/bin/sh", "-c", "kill -TERM $$"}, quiet, quiet, os.Stderr)
	skipUnlessNamespaced(t, err)
	if err != nil {
		t.Fatalf("the session failed: %v", err)
	}
	if status != 128+int(syscall.SIGTERM) {
		t.Errorf("the session exited %d, wanted %d", status, 128+int(syscall.SIGTERM))
	}
}

// The composition really exists inside, and nothing of it exists outside.
func TestTheCompositionIsBuiltInsideAndInvisibleOutside(t *testing.T) {
	session := prepare(t)

	output, err := os.CreateTemp(session.Env.Path, "out-")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()

	quiet := devnull(t)
	status, err := session.start([]string{"/bin/sh", "-c", "ls -a"}, quiet, output, os.Stderr)
	skipUnlessNamespaced(t, err)
	if err != nil || status != 0 {
		t.Fatalf("the session failed: status=%d err=%v", status, err)
	}

	listing, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CLAUDE.md", "src", ".claude"} {
		if !strings.Contains(string(listing), name) {
			t.Errorf("the composed tree did not show %q:\n%s", name, listing)
		}
	}

	// And outside, the live directory is empty and always was.
	entries, err := os.ReadDir(session.Env.Live)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the live directory is not empty outside the session: %v", entries)
	}
}

// -- the declared environment, in a real namespace --------------------------

// declaringSentinel builds a composition that declares a sentinel and
// prepends a launcher directory living in the workspace repository, which
// is the arrangement the documentation recommends.
func declaringSentinel(t *testing.T, sentinel string) *ready {
	t.Helper()
	env := testenv.NewEnv(t)

	// A workspace-owned executable, reachable only through the declared
	// PATH. It has to exist before the snapshot is taken, or the inventory
	// would refuse the composition for a root entry nobody accepted.
	launcher := filepath.Join(env.Workspace, ".workspace", "bin", "camp-test-launcher")
	testenv.Write(t, launcher, "#!/bin/sh\necho launcher-ran\n")
	if err := os.Chmod(launcher, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TEST_SENTINEL_SOURCE", sentinel)
	return prepareWith(t, env, env.YAML()+`
session:
  environment:
    SESSION_SENTINEL: "$TEST_SENTINEL_SOURCE"
    PATH: "$CAMP_LIVE/.workspace/bin:$PATH"
`)
}

// output runs a workload and returns what it wrote.
func (r *ready) output(t *testing.T, argv []string) (string, int, error) {
	t.Helper()
	file, err := os.CreateTemp(r.Env.Path, "out-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	status, err := r.start(argv, devnull(t), file, os.Stderr)
	if err != nil {
		return "", status, err
	}
	data, readErr := os.ReadFile(file.Name())
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(data), status, nil
}

// What the whole section is for: a declared value reaches the workload,
// and the process that mounted the composition never had it.
func TestADeclaredValueReachesTheWorkloadAndNotTheInit(t *testing.T) {
	const sentinel = "sentinel-value-9c1f"
	session := declaringSentinel(t, sentinel)

	// A direct workload, an interactive-style shell, and a descendant that
	// detached itself: all three are reached by ordinary inheritance, and
	// all three are how somebody actually works in here.
	script := `printf 'direct=%s\n' "$SESSION_SENTINEL"
/bin/sh -c 'printf "shell=%s\n" "$SESSION_SENTINEL"'
setsid /bin/sh -c 'printf "daemon=%s\n" "$SESSION_SENTINEL" >&1'
printf 'init=%s\n' "$(tr '\0' '\n' < /proc/1/environ | grep -c '^SESSION_SENTINEL=')"
printf 'caps=%s\n' "$(grep ^CapEff /proc/self/status)"`

	text, status, err := session.output(t, []string{"/bin/sh", "-c", script})
	skipUnlessNamespaced(t, err)
	if err != nil || status != 0 {
		t.Fatalf("the session failed: status=%d err=%v", status, err)
	}

	for _, want := range []string{
		"direct=" + sentinel,
		"shell=" + sentinel,
		"daemon=" + sentinel,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the declared value did not reach one of them (%q):\n%s", want, text)
		}
	}
	if !strings.Contains(text, "init=0") {
		t.Errorf("the sentinel is in camp-as-init's own environment. Nothing "+
			"declared may touch the process that held the mount capability:\n%s", text)
	}
	if !strings.Contains(text, "CapEff:\t0000000000000000") {
		t.Errorf("the workload holds capabilities; the drop happens before "+
			"anything the user asked for runs:\n%s", text)
	}
}

// The declared PATH selects the command. This is the composition-owned
// launcher pattern working end to end: a program that has no option
// variable of its own is reached through a directory the workspace
// provides, and camp neither writes nor blesses what is in it.
func TestABareCommandIsFoundThroughTheDeclaredPathInASession(t *testing.T) {
	session := declaringSentinel(t, "unused")

	text, status, err := session.output(t, []string{"camp-test-launcher"})
	skipUnlessNamespaced(t, err)
	if err != nil || status != 0 {
		t.Fatalf("the session failed: status=%d err=%v", status, err)
	}
	if !strings.Contains(text, "launcher-ran") {
		t.Errorf("the workspace launcher was not the command that ran:\n%s", text)
	}
}

// Nothing declared survives the session, and neither does the tree.
func TestNothingDeclaredLeaksOutOfTheSession(t *testing.T) {
	// The snapshot is taken after the fixture is built, not before: the
	// fixture sets the variable the declaration reads, and comparing across
	// that would fail on the test's own doing rather than on camp's.
	session := declaringSentinel(t, "sentinel-value-9c1f")
	before := strings.Join(os.Environ(), "\n")

	_, status, err := session.output(t, []string{"/bin/sh", "-c", "true"})
	skipUnlessNamespaced(t, err)
	if err != nil || status != 0 {
		t.Fatalf("the session failed: status=%d err=%v", status, err)
	}

	if after := strings.Join(os.Environ(), "\n"); after != before {
		t.Error("the environment of the process that started the session changed")
	}
	if os.Getenv("SESSION_SENTINEL") != "" {
		t.Error("a declared variable escaped into the calling process")
	}
	entries, err := os.ReadDir(session.Env.Live)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the live directory is not empty outside the session: %v", entries)
	}
}

// The property the whole supervisor exists for: a daemonising workload
// returns at once, the init stays resident, and it is the init that holds
// the locks -- not the workload, whose habits with inherited descriptors
// are nobody's to rely on.
func TestADaemonisedWorkloadReturnsWhileTheInitHoldsTheLocks(t *testing.T) {
	session := prepare(t)
	quiet := devnull(t)

	marker := filepath.Join(session.Env.Path, "still-running")
	// setsid detaches it from this process group and it closes its
	// descriptors, exactly as a tmux server does.
	daemon := []string{"/bin/sh", "-c",
		"setsid /bin/sh -c 'sleep 20 >/dev/null 2>&1 </dev/null' & echo started > " + marker}

	started := time.Now()
	status, err := session.start(daemon, quiet, quiet, os.Stderr)
	skipUnlessNamespaced(t, err)
	if err != nil {
		t.Fatalf("the session failed: %v", err)
	}
	if status != 0 {
		t.Fatalf("the launching command exited %d", status)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("the launcher waited %s for a daemonised workload", elapsed)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the workload never ran: %v", err)
	}

	// The launcher has returned. The init is still in there, and a second
	// composition on the same upper must still be refused.
	session.Locks.Release()
	if _, err := takeLocks(session.Config); err == nil {
		t.Error("a second composition was accepted while the session was still " +
			"running.\nThe init is camp resident as pid 1 of the namespace and it " +
			"holds the locks: a daemonising workload routinely closes the " +
			"descriptors it inherited, so the design must not depend on the " +
			"workload keeping them")
	}

}
