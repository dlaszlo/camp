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
	built, refused := plan.Prepare(cfg)
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
		Config: r.Config,
		Argv:   argv,
		Locks:  r.Locks,
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
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

// -- the end of a session ------------------------------------------------------
//
// A session ends when its workload ends: the shell or the command camp
// started for it. What is still inside at that moment is named, asked to
// leave once, given Grace, and then left to the kernel. Every test here
// measures one clause of that in a real namespace.

// captured is a file a session's stderr is written to, so a test can read
// what the init said.
func captured(t *testing.T, env *testenv.Env) *os.File {
	t.Helper()
	file, err := os.CreateTemp(env.Path, "stderr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file
}

func contents(t *testing.T, file *os.File) string {
	t.Helper()
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// lockable reports whether the session's locks can be taken again, which
// is the kernel having released them: the init has exited.
func lockable(cfg config.Config) bool {
	pair, err := takeLocks(cfg)
	if err != nil {
		return false
	}
	pair.Release()
	return true
}

// The rule itself. A workload that daemonises a child and exits used to
// leave the session standing for as long as the child lived; now the
// session ends with the workload, and the child is asked -- SIGTERM, which
// it can act on -- rather than killed.
func TestASessionEndsWhenItsWorkloadDoesAndAsksTheRestToLeave(t *testing.T) {
	composition := prepare(t)
	stderr := captured(t, composition.Env)
	quiet := devnull(t)

	// setsid detaches it from the workload's process group, exactly as a
	// tmux server does, and the workload exits leaving it behind -- once the
	// child says it is ready, or the request would meet a shell that has not
	// installed its trap yet and dies of the signal instead of recording it.
	// The child traps SIGTERM and records that it was asked.
	marker := filepath.Join(composition.Env.Path, "asked")
	ready := filepath.Join(composition.Env.Path, "ready")
	daemon := []string{"/bin/sh", "-c",
		"setsid /bin/sh -c 'trap \"echo term > " + marker + "; exit 0\" TERM; " +
			"echo > " + ready + "; sleep 300 & wait' >/dev/null 2>&1 </dev/null & " +
			"until [ -e " + ready + " ]; do sleep 0.05; done"}

	started := time.Now()
	status, err := composition.start(daemon, quiet, quiet, stderr)
	skipUnlessNamespaced(t, err)
	if err != nil {
		t.Fatalf("the session failed: %v", err)
	}
	if status != 0 {
		t.Fatalf("the launching command exited %d; the status is the workload's "+
			"own, whatever the session did afterwards", status)
	}
	if elapsed := time.Since(started); elapsed >= session.Grace {
		t.Errorf("the session took %s to end after a workload that exited at "+
			"once; a child that acts on SIGTERM has to end it well inside the "+
			"grace of %s", elapsed, session.Grace)
	}
	if data, err := os.ReadFile(marker); err != nil || !strings.Contains(string(data), "term") {
		t.Errorf("the daemonised child was not asked with SIGTERM (marker: %v, "+
			"%q). The end of a session is a request first", err, data)
	}
	if !lockable(composition.Config) {
		t.Error("the launcher has returned and the locks are still held: the init " +
			"outlived its workload, which is the state this rule exists to remove")
	}

	said := contents(t, stderr)
	for _, want := range []string{
		"the command has exited, and", "still running",
		"SIGTERM, with SIGCONT", "pid ", "waits up to 10 seconds", "nothing stronger",
	} {
		if !strings.Contains(said, want) {
			t.Errorf("the init did not say %q before asking what was inside to "+
				"leave:\n%s", want, said)
		}
	}
	if strings.Contains(said, "after 10 seconds") {
		t.Errorf("the deadline message was printed although the namespace "+
			"emptied in time:\n%s", said)
	}
}

// A process that ignores the request is not killed by camp. At the
// deadline the init names it, says the kernel will end it, and exits; the
// launcher returns the workload's status, not a verdict on the session.
func TestAProcessThatIgnoresTheRequestIsNamedAndLeftToTheKernel(t *testing.T) {
	composition := prepare(t)
	stderr := captured(t, composition.Env)
	quiet := devnull(t)

	// An ignored disposition survives execve, so the sleep ignores SIGTERM
	// too. The workload waits until it is in place before exiting.
	ready := filepath.Join(composition.Env.Path, "ready")
	daemon := []string{"/bin/sh", "-c",
		"setsid /bin/sh -c 'trap \"\" TERM; echo > " + ready + "; exec sleep 300' " +
			">/dev/null 2>&1 </dev/null & until [ -e " + ready + " ]; do sleep 0.05; done"}

	started := time.Now()
	status, err := composition.start(daemon, quiet, quiet, stderr)
	skipUnlessNamespaced(t, err)
	if err != nil {
		t.Fatalf("the session failed: %v", err)
	}
	elapsed := time.Since(started)
	if status != 0 {
		t.Errorf("the launching command exited %d; a session that did not empty "+
			"in time is reported on stderr, never folded into the workload's "+
			"status", status)
	}
	if elapsed < session.Grace {
		t.Errorf("the session ended after %s with a process inside that ignores "+
			"SIGTERM; it has to wait the whole grace of %s before giving up on it",
			elapsed, session.Grace)
	}
	if elapsed > session.Grace+10*time.Second {
		t.Errorf("the session took %s to end; the deadline is %s and nothing "+
			"waits past it", elapsed, session.Grace)
	}
	if !lockable(composition.Config) {
		t.Error("the launcher has returned and the locks are still held")
	}

	said := contents(t, stderr)
	for _, want := range []string{
		"after 10 seconds, 1 process(es) are still in the session",
		"sleep 300", "camp does not kill them", "SIGKILL",
	} {
		if !strings.Contains(said, want) {
			t.Errorf("the deadline message does not say %q:\n%s", want, said)
		}
	}
}

// A shell that exits with nothing behind it produces nothing new: no
// paragraph about processes, no deadline, and a farewell whose git reads
// ran -- against a code repository whose own path is frozen read-only for
// the whole session, which the end-of-session pass has to read through.
func TestAnOrdinaryExitSaysNothingNew(t *testing.T) {
	composition := prepare(t)
	stderr := captured(t, composition.Env)
	quiet := devnull(t)

	started := time.Now()
	status, err := composition.start([]string{"/bin/sh", "-c", "true"}, quiet, quiet, stderr)
	skipUnlessNamespaced(t, err)
	if err != nil || status != 0 {
		t.Fatalf("the session failed: status=%d err=%v", status, err)
	}
	if elapsed := time.Since(started); elapsed >= session.Grace {
		t.Errorf("an empty namespace took %s to be found empty", elapsed)
	}

	said := contents(t, stderr)
	for _, unwanted := range []string{"still running", "after 10 seconds", "could not run"} {
		if strings.Contains(said, unwanted) {
			t.Errorf("the init said %q after a clean exit with nothing left "+
				"inside:\n%s", unwanted, said)
		}
	}
}

// A signal to the init converges on the same ending. It fans out, the
// workload dies of it, and the workload's exit is what ends the session --
// with what was daemonised beside it asked, once, in the same fan-out.
//
// This is the 2026-08-28 test rewritten for the new rule: the same
// daemonised process, the same signal, and one fewer wait, because the
// workload no longer has to have left for the daemon to matter.
func TestASignalToTheInitEndsTheSessionThroughItsWorkload(t *testing.T) {
	composition := prepare(t)
	quiet := devnull(t)
	live := composition.Config.Live()

	// A workload that stays, with a daemon detached beside it. It says when
	// it is running, and that is what the test synchronises on: the init
	// starts the workload only after it has subscribed to SIGTERM, so a
	// workload that has run proves the handler is in place -- the lock
	// descriptors alone do not, since the init holds them from before the
	// first mount, and a signal during mounting meets the runtime's own
	// disposition instead of the supervisor's.
	ready := filepath.Join(composition.Env.Path, "running")
	workload := []string{"/bin/sh", "-c",
		"setsid /bin/sh -c 'sleep 300' >/dev/null 2>&1 </dev/null & echo > " + ready +
			"; exec sleep 300"}

	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		status, err := composition.start(workload, quiet, quiet, os.Stderr)
		done <- result{status, err}
	}()

	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case outcome := <-done:
			skipUnlessNamespaced(t, outcome.err)
			t.Fatalf("the session ended before its workload ran: status=%d err=%v",
				outcome.status, outcome.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the workload had not run after 20s")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The init is found the way camp's own refusal finds it -- as the
	// process holding the live lock -- and told apart from this process's
	// own copies by the argument only the init is invoked with.
	pid := 0
	for _, holder := range locks.Holders(live) {
		if strings.Contains(holder.Command, session.InitArg) {
			pid = holder.PID
		}
	}
	if pid == 0 {
		t.Fatalf("no init holds the live lock %s while the workload is running", live)
	}

	signalled := time.Now()
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatalf("signalling the init (pid %d): %v", pid, err)
	}
	outcome := <-done
	if outcome.err != nil {
		t.Fatalf("the session failed: %v", outcome.err)
	}
	if outcome.status != 128+int(syscall.SIGTERM) {
		t.Errorf("the launcher returned %d, wanted %d: the workload died of the "+
			"SIGTERM the init fanned out, and its status is the shell's convention",
			outcome.status, 128+int(syscall.SIGTERM))
	}
	if elapsed := time.Since(signalled); elapsed >= session.Grace {
		t.Errorf("the session took %s to end after SIGTERM; the daemon beside the "+
			"workload was asked in the same fan-out and dies of it at once", elapsed)
	}
	if !lockable(composition.Config) {
		t.Error("the session's locks are still held after the launcher returned")
	}
}

// A stopped process keeps a SIGTERM pending and stays stopped: it never
// gets to decide anything. The ending's fan-out is followed by SIGCONT,
// which is what lets it act; without that the session would stand until
// the deadline and the kernel would end it.
func TestTheEndingReachesAStoppedProcessToo(t *testing.T) {
	composition := prepare(t)
	quiet := devnull(t)

	// The workload exits only once the child is really stopped -- read from
	// the namespace's own /proc, whose pids are what $$ gave -- or the
	// request would meet a running process and prove nothing about SIGCONT.
	pidfile := filepath.Join(composition.Env.Path, "stopped")
	daemon := []string{"/bin/sh", "-c",
		"setsid /bin/sh -c 'echo $$ > " + pidfile + "; kill -STOP $$; sleep 300' " +
			">/dev/null 2>&1 </dev/null & " +
			"until [ -e " + pidfile + " ] && grep -q '^State:.T' /proc/$(cat " + pidfile +
			")/status 2>/dev/null; do sleep 0.05; done"}

	started := time.Now()
	status, err := composition.start(daemon, quiet, quiet, os.Stderr)
	skipUnlessNamespaced(t, err)
	if err != nil {
		t.Fatalf("the session failed: %v", err)
	}
	if status != 0 {
		t.Fatalf("the launching command exited %d", status)
	}
	if elapsed := time.Since(started); elapsed >= session.Grace {
		t.Errorf("the session took %s to end with a stopped process inside.\nA "+
			"stopped process cannot act on a signal it is holding pending: "+
			"without a SIGCONT after the fan-out it stays stopped until the "+
			"deadline, and the kernel ends it instead of it ending itself", elapsed)
	}
	if !lockable(composition.Config) {
		t.Error("the session's locks are still held after the launcher returned")
	}
}

// -- the code repository's own path, inside a real session --------------------

// No descriptor of the code repository, the composed tree's directory or
// the handshake pipe reaches the workload. The lock descriptors were
// opened through the raw path before the freeze existed, and a path
// resolved through a descriptor uses the mount the descriptor was opened
// on -- so a workload holding one wrote the code repository at
// /proc/self/fd/4/... behind the frozen path, measured, while the
// polarity check passed. The lock lives on the open file description the
// init keeps; the child gets no copy.
func TestTheWorkloadInheritsNoLockOrPipeDescriptor(t *testing.T) {
	composition := prepare(t)
	upper := composition.Config.UpperPath()
	live := composition.Config.Live()

	// Every descriptor above the three standard ones, with its target, then
	// the write the leak allowed. The write goes through the descriptor
	// number the init holds the upper lock on; with no descriptor there it
	// cannot resolve at all. The standard three are the test's own -- under
	// go test, stderr is a pipe -- and say nothing about the init.
	script := `for fd in /proc/self/fd/*; do
  n=${fd##*/}; [ "$n" -gt 2 ] && echo "fd $n -> $(readlink "$fd")"
done
echo x > /proc/self/fd/4/via-descriptor 2>/dev/null; echo via=$?`
	text, status, err := composition.output(t, []string{"/bin/sh", "-c", script})
	skipUnlessNamespaced(t, err)
	if err != nil || status != 0 {
		t.Fatalf("the session failed: status=%d err=%v\n%s", status, err, text)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasSuffix(line, "-> "+upper) || strings.HasSuffix(line, "-> "+live) {
			t.Errorf("the workload holds a descriptor for the locked directory (%s). "+
				"The lock descriptors stay with the init: through one of them the "+
				"code repository is reachable writable behind the frozen path", line)
		}
		if strings.Contains(line, "-> pipe:") {
			t.Errorf("the workload holds a pipe descriptor (%s); the handshake pipe "+
				"is the init's", line)
		}
	}
	if !strings.Contains(text, "via=") || strings.Contains(text, "via=0") {
		t.Errorf("a write through /proc/self/fd/4 succeeded inside the session:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(upper, "via-descriptor")); err == nil {
		t.Error("the write through the inherited descriptor landed in the code repository")
	}
}

// Inside a session the code repository's raw path refuses writes, and the
// same write through the composed tree lands. The guard exists so that a
// write behind the overlay's back -- which freezes what the tree shows at
// that path for the rest of the session -- meets EROFS instead.
func TestTheCodeRepositorysOwnPathRefusesWritesInsideASession(t *testing.T) {
	composition := prepare(t)
	upper := composition.Config.UpperPath()

	script := `echo x > "$1"/behind-the-back 2>/dev/null; echo raw=$?
echo y > ./through-the-tree && echo tree=ok && rm -f ./through-the-tree`
	text, status, err := composition.output(t, []string{"/bin/sh", "-c", script, "sh", upper})
	skipUnlessNamespaced(t, err)
	if err != nil || status != 0 {
		t.Fatalf("the session failed: status=%d err=%v\n%s", status, err, text)
	}
	if !strings.Contains(text, "raw=") || strings.Contains(text, "raw=0") {
		t.Errorf("a write to the code repository through its own path succeeded "+
			"inside the session:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(upper, "behind-the-back")); err == nil {
		t.Error("the write behind the overlay's back landed in the code repository")
	}
	if !strings.Contains(text, "tree=ok") {
		t.Errorf("a write through the composed tree failed:\n%s", text)
	}
}

// The configuration is read twice: once by the launcher, which takes the
// locks from it, and once by the init, which mounts from it. A file
// edited in between would have the init compose one tree while camp holds
// the locks for another -- two compositions on one upper, which is what
// the locks exist to make impossible.
//
// The init cannot answer that by trusting the launcher: it is the process
// that mounts, so it derives the plan itself. What it can do is ask the
// two lock descriptors it inherited what they are on.
func TestASessionRefusesToMountSomethingItsLocksAreNotOn(t *testing.T) {
	ready := prepare(t)

	// Another empty composed tree, and a configuration that names it. The
	// locks were taken on the first one a moment ago.
	elsewhere := filepath.Join(ready.Env.Path, "live-elsewhere")
	testenv.MkDir(t, elsewhere)
	swapped := strings.Replace(ready.Env.YAML(), "merged: live", "merged: live-elsewhere", 1)
	if err := os.WriteFile(ready.Config.Source, []byte(swapped), 0o644); err != nil {
		t.Fatal(err)
	}

	quiet := devnull(t)
	_, err := ready.start([]string{"/bin/sh", "-c", "true"}, quiet, quiet, quiet)
	skipUnlessNamespaced(t, err)
	if err == nil {
		t.Fatal("the session composed a tree the locks it holds are not on")
	}
	if !strings.Contains(err.Error(), "lock this session holds") {
		t.Errorf("the session failed for some other reason:\n%v", err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("something was mounted at the swapped tree: %v", entries)
	}
}
