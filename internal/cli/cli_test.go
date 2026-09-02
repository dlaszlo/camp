package cli_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/cli"
	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/enc"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/locks"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/preflight"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/session"
	"github.com/dlaszlo/camp/internal/testenv"
)

// insideEnv carries a configuration path into a workload started by
// TestEnteringFromInsideIsRefusedBeforeAnythingSweeps: the test binary,
// running inside a session, becomes the 'camp shell' typed there.
const insideEnv = "CAMP_CLI_SHELL_FROM_INSIDE"

// shellReport is what that workload sends back.
type shellReport struct {
	Code   int    `json:"code"`
	Stderr string `json:"stderr"`
}

func shellFromInside(source string) int {
	var out, errOut bytes.Buffer
	code := cli.Main([]string{"shell", "-f", source}, &out, &errOut)
	encoded, _ := json.Marshal(shellReport{Code: code, Stderr: errOut.String()})
	os.Stdout.Write(encoded)
	return 0
}

// TestMain answers the two hidden arguments this binary is re-executed
// with, the way cmd/camp does, before any test runs.
func TestMain(m *testing.M) {
	// The capability probe, answered before anything else, exactly as
	// cmd/camp answers it and for the same reason: it has one job and
	// nothing may run before it.
	//
	// preflight starts that probe as os.Executable(), which from here is
	// this test binary. Without this line the child does not recognise
	// the argument, runs the whole package again inside the new user
	// namespace it was given, and every copy starts another. On this host
	// the clone fails and it never showed; inside a composition, where
	// the namespace is permitted, the run does not end. Measured:
	// 'camp run -- go test ./internal/... -timeout 90s' stopped in
	// TestDoctorSaysWhenTheMountTableCannotBeRead with two pipe readers
	// waiting on a child that was running the suite.
	if len(os.Args) > 1 && os.Args[1] == preflight.ProbeArg {
		os.Exit(preflight.Probe())
	}
	// The same binary is the session's init when a test here starts one,
	// and the workload inside it.
	if len(os.Args) > 1 && os.Args[1] == session.InitArg {
		session.InitMain(os.Args[2:])
		return
	}
	if source := os.Getenv(insideEnv); source != "" {
		os.Exit(shellFromInside(source))
	}
	os.Exit(m.Run())
}

// run invokes a command the way a terminal does, and returns what each
// stream received.
func run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Main(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// refusing builds an environment whose composition cannot start: the same
// name at both repository roots, which the gate refuses. The plan itself
// still derives, which is exactly the situation this test is about.
func refusing(t *testing.T) string {
	t.Helper()
	env := testenv.NewEnv(t)
	testenv.Write(t, filepath.Join(env.Code, "AGENTS.md"), "the code's own\n")
	env.Config(t, "")
	return config.Path(env.Path)
}

// explain describes a tree to whoever is standing in it. Rendered beside a
// standing refusal it would describe a tree that will not exist, and a
// description reads as authority in a way a plan does not -- so every
// refusal stops it, not only the ones that left no plan behind.
func TestExplainDescribesNothingWhileARefusalStands(t *testing.T) {
	path := refusing(t)

	out, errOut, code := run(t, "explain", "-f", path)
	if code == 0 {
		t.Error("explain exited 0 with a refusal standing")
	}
	if strings.Contains(out, "You are in") {
		t.Errorf("explain described the tree anyway:\n%s", out)
	}
	if !strings.Contains(errOut, "AGENTS.md") {
		t.Errorf("explain did not name what stops the composition:\n%s", errOut)
	}
}

// A file that no longer parses stops every command that has to understand
// it -- the session: section adds a whole class of ways to get that wrong
// -- and each of them names the entry it could not read.
func TestABrokenSectionStillStopsTheCommandsThatNeedIt(t *testing.T) {
	env := testenv.NewEnv(t)
	env.Config(t, "")
	path := config.Path(env.Path)

	broken, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	testenv.Write(t, path, string(broken)+"\nsession:\n  environment:\n    PORT: 8080\n")

	for _, command := range [][]string{
		{"plan", "-f", path},
		{"explain", "-f", path},
		{"status", "-f", path},
	} {
		_, errOut, code := run(t, command...)
		if code == 0 {
			t.Errorf("%v accepted a configuration it cannot read", command)
		}
		if !strings.Contains(errOut, "PORT") {
			t.Errorf("%v did not name the entry it could not read:\n%s", command, errOut)
		}
	}
}

// plan is the other half of the same rule, and it behaves differently on
// purpose: it prints the derived plan and then what stops it, because the
// plan is what somebody repairing the configuration needs to look at.
func TestPlanStillPrintsThePlanBesideTheRefusal(t *testing.T) {
	path := refusing(t)

	out, errOut, code := run(t, "plan", "-f", path)
	if code == 0 {
		t.Error("plan exited 0 with a refusal standing")
	}
	if !strings.Contains(out, "mount sequence, in order:") {
		t.Errorf("plan printed no sequence:\n%s", out)
	}
	// The two are printed together and go to different streams: the plan is
	// the command's product, which somebody pipes, and what stops it is
	// about the run.
	if !strings.Contains(errOut, "would not start") {
		t.Errorf("plan did not say the composition would not start:\n%s", errOut)
	}
	if strings.Contains(out, "would not start") {
		t.Errorf("the refusal is on stdout, where the plan is:\n%s", out)
	}
}

// A plan says what a run would do. The prepare commands are the one part
// of a real run that changes something before any mount, so a plan that
// listed the mounts and said nothing about them would describe a
// different run -- and a plan that ran them would be executing something,
// which planning never does.
func TestPlanNamesThePrepareCommandsAndRunsNone(t *testing.T) {
	env := testenv.NewEnv(t)
	marker := filepath.Join(env.Path, "it-ran")
	env.Config(t, env.YAML()+"\nprepare:\n  - command: [\"/bin/sh\", \"-c\", "+
		"\": > "+marker+"\"]\n")

	out, _, _ := run(t, "plan", "-f", config.Path(env.Path))
	if !strings.Contains(out, "prepare, before anything is composed:") {
		t.Errorf("plan said nothing about the prepare commands:\n%s", out)
	}
	if !strings.Contains(out, "has not run them") {
		t.Errorf("plan has to say it did not run them:\n%s", out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("'camp plan' ran a prepare command. Planning executes nothing")
	}
}

// Where the prepare commands sit in the frame, measured through the
// command that composes: they run after the locks and before the plan is
// derived, and one that fails stops the composition with nothing mounted.
//
// The fixture proves the order rather than asserting it. The gate would
// refuse this composition anyway -- the same name stands at both
// repository roots -- so a marker written by a prepare command can only
// exist if the commands ran before the validation that refuses.
func TestThePrepareCommandsRunBeforeAnythingIsDerived(t *testing.T) {
	env := testenv.NewEnv(t)
	testenv.Write(t, filepath.Join(env.Code, "AGENTS.md"), "the code's own\n")
	marker := filepath.Join(env.Path, "the-first-one-ran")
	env.Config(t, env.YAML()+"\nprepare:\n"+
		"  - command: [\"/bin/sh\", \"-c\", \": > "+marker+"\"]\n"+
		"  - command: [\"/bin/sh\", \"-c\", \"exit 3\"]\n")

	_, errOut, code := run(t, "run", "-f", config.Path(env.Path), "--", "/bin/true")
	if strings.Contains(errOut, "this machine cannot run camp") {
		t.Skipf("this machine refuses the namespace camp needs, so no command "+
			"that composes gets as far as the prepare commands:\n%s", errOut)
	}
	if code == 0 {
		t.Fatalf("a failing prepare command let the session start:\n%s", errOut)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("the first prepare command did not run before the validation " +
			"that refuses this composition. They have to run first: one of them " +
			"may change a repository, and the gate has to read the repositories " +
			"as they will be mounted")
	}
	if !strings.Contains(flat(errOut), "prepare command 2") {
		t.Errorf("the refusal does not name the command that failed:\n%s", errOut)
	}
	// The gate's own refusal is not what stopped this: the prepare command
	// did, and the composition never got as far as being validated.
	if strings.Contains(errOut, "AGENTS.md") {
		t.Errorf("the run went on to validate after a prepare command "+
			"failed:\n%s", errOut)
	}
}

// The reason the prepare commands run before the plan is derived, stated
// as the thing that would break if they did not: what one of them changes
// has to be what camp then validates.
//
// Here a prepare command creates a name in the code repository that the
// workspace also has at its root. The gate refuses that overlap -- and it
// can only refuse it if it read the repository after the command ran.
func TestARepositoryChangeMadeByPrepareIsSeenByTheGate(t *testing.T) {
	env := testenv.NewEnv(t)
	env.Config(t, env.YAML()+"\nprepare:\n  - command: [\"/bin/sh\", \"-c\", "+
		"\"echo the-code-repositorys-own > "+filepath.Join(env.Code, "AGENTS.md")+"\"]\n")

	_, errOut, code := run(t, "run", "-f", config.Path(env.Path), "--", "/bin/true")
	if skipUnlessComposable(t, errOut) {
		return
	}
	if code == 0 {
		t.Fatalf("the composition started with both roots holding AGENTS.md:\n%s", errOut)
	}
	if !strings.Contains(errOut, "AGENTS.md") {
		t.Errorf("the gate did not see what the prepare command wrote, so it "+
			"read the repositories before the command ran:\n%s", errOut)
	}
}

// A prepare command runs between camp locking two directories and camp
// deriving a plan from them, so it is able to rename one and put another
// directory at the same path. camp would then hold a lock on an inode
// nothing mounts, and build a composition no lock protects.
func TestPrepareCannotReplaceTheDirectoryCampLocked(t *testing.T) {
	env := testenv.NewEnv(t)
	env.Config(t, env.YAML()+"\nprepare:\n  - command: [\"/bin/sh\", \"-c\", "+
		"\"mv "+env.Code+" "+env.Code+".moved && mkdir "+env.Code+"\"]\n")

	_, errOut, code := run(t, "run", "-f", config.Path(env.Path), "--", "/bin/true")
	if skipUnlessComposable(t, errOut) {
		return
	}
	if code == 0 {
		t.Fatalf("camp composed a code repository it had not locked:\n%s", errOut)
	}
	if !strings.Contains(flat(errOut), "different directory from the one camp locked") {
		t.Errorf("the refusal does not say what happened to the lock:\n%s", errOut)
	}
}

// The same window reaches the configuration itself: camp has read it,
// taken the locks from it and run the commands it declares, and the
// process that mounts reads it again for itself.
func TestTheConfigurationCannotChangeWhileThePrepareCommandsRun(t *testing.T) {
	env := testenv.NewEnv(t)
	path := config.Path(env.Path)
	env.Config(t, env.YAML()+"\nprepare:\n  - command: [\"/bin/sh\", \"-c\", "+
		"\"printf '# edited from inside a prepare command\\n' >> "+path+"\"]\n")

	_, errOut, code := run(t, "run", "-f", path, "--", "/bin/true")
	if skipUnlessComposable(t, errOut) {
		return
	}
	if code == 0 {
		t.Fatalf("camp planned from a file that had changed under it:\n%s", errOut)
	}
	if !strings.Contains(flat(errOut), "changed while the prepare commands were running") {
		t.Errorf("the refusal does not say the file changed under the run:\n%s", errOut)
	}
}

// flat folds a report's terminal wrapping away, so that a test can look
// for a sentence rather than for the column it happened to break at.
func flat(text string) string { return strings.Join(strings.Fields(text), " ") }

// skipUnlessComposable skips a test whose subject is only reached by a
// command that builds a composition, on a machine that refuses the
// namespace camp needs.
func skipUnlessComposable(t *testing.T, errOut string) bool {
	t.Helper()
	if strings.Contains(errOut, "this machine cannot run camp") {
		t.Skipf("this machine refuses the namespace camp needs, so no command "+
			"that composes gets this far:\n%s", errOut)
		return true
	}
	return false
}

// init writes the skeleton into camp's own directory inside the
// environment, and there was nothing holding it there.
//
// It took the configuration's path and went one directory up for the base
// -- which is $ENV/.camp, camp's own directory, not the environment. So
// the area became $ENV/.camp/.camp: the command needed a directory it was
// supposed to create, failed on a fresh environment for that reason, and
// on an environment where .camp happened to exist would have written the
// three files a level too deep while printing the path it did not write.
func TestInitWritesTheSkeletonIntoTheEnvironment(t *testing.T) {
	env := t.TempDir()

	out, errOut, code := run(t, "init", env)
	if code != 0 {
		t.Fatalf("init exited %d in an empty directory:\n%s\n%s", code, out, errOut)
	}
	for _, name := range []string{"config.yml", ".gitignore", "README.md"} {
		if _, err := os.Stat(filepath.Join(env, config.Dir, name)); err != nil {
			t.Errorf("%s is not where init said it wrote it: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(env, config.Dir, config.Dir)); err == nil {
		t.Errorf("init made a second %s inside camp's own directory", config.Dir)
	}
	if !strings.Contains(out, config.Path(env)) {
		t.Errorf("init did not name the file it wrote:\n%s", out)
	}
}

// A configuration camp refuses is one of the runs most worth having a
// record of, and the record is the environment's own log.
//
// The log lives under the environment's .camp, and until the file parses
// nothing has said where that is -- except the path of the file itself,
// which for the layout camp writes is two components below it. So the
// log is attached from the path, before the parse, and the refusal that
// follows lands in it. Attached after the parse, as it was, the whole
// refusal reached the terminal and nothing else.
func TestARefusedConfigurationIsKeptInTheEnvironmentsLog(t *testing.T) {
	env := testenv.NewEnv(t)
	path := config.Path(env.Path)
	// Standard place, and not a configuration: an unknown key, which camp
	// refuses rather than guesses at.
	testenv.Write(t, path, "env: "+env.Path+"\nmerged: live\nsandbox: true\n")

	_, errOut, code := run(t, "plan", "-f", path)
	if code == 0 {
		t.Fatalf("a configuration with an unknown key was accepted:\n%s", errOut)
	}

	kept, err := os.ReadFile(filepath.Join(env.Path, ".camp", "logs", "camp.log"))
	if err != nil {
		t.Fatalf("the refusal was not written to the environment's log: %v", err)
	}
	// Every line of it, not the fact that something was written: what is
	// worth having a week later is what the person at the terminal saw.
	for _, line := range strings.Split(strings.TrimRight(errOut, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.Contains(string(kept), line) {
			t.Errorf("the log does not carry a line the terminal was given:\n"+
				"  %q\nlog:\n%s", line, kept)
		}
	}
	if !strings.Contains(string(kept), "sandbox") {
		t.Errorf("the log does not name the key that was refused:\n%s", kept)
	}
}

// A -f pointing outside the layout camp writes attaches no log, and the
// command still runs.
//
// The environment root is derived from the configuration's path by
// removing two components, which is true of $ENV/.camp/config.yml and of
// nothing else. For /tmp/mine.yml it would make the root "/" and camp
// would try to write its log into /.camp -- somebody else's directory,
// and not this composition's. There the parsed configuration is what
// says which environment this is.
func TestAConfigurationOutsideTheLayoutIsLoggedWhereItSaysToLog(t *testing.T) {
	env := testenv.NewEnv(t)
	// Accepted the way every fixture is, without writing a configuration
	// into the environment: the only one there is is the one -f names.
	cfg, err := env.TryConfig(env.YAML())
	if err != nil {
		t.Fatalf("the fixture configuration did not parse:\n%v", err)
	}
	env.Accept(t, cfg)
	// Two components below a directory of its own, which is exactly the
	// shape the derivation would misread: strip two and it names that
	// directory, which is nobody's environment root.
	outside := testenv.Root(t)
	elsewhere := filepath.Join(outside, "somewhere", "mine.yml")
	testenv.Write(t, elsewhere, env.YAML())

	_, errOut, code := run(t, "plan", "-f", elsewhere)
	if code != 0 {
		t.Fatalf("the composition was refused:\n%s", errOut)
	}
	// Nothing was written where the derivation would have put it ...
	if _, err := os.Stat(filepath.Join(outside, ".camp")); err == nil {
		t.Errorf("camp made a .camp in %s, which is not an environment root: "+
			"the log was written two components above a file that is not in "+
			"camp's own layout", outside)
	}
	// ... and the log is where env: says the environment is.
	if _, err := os.Stat(filepath.Join(env.Path, ".camp", "logs", "camp.log")); err != nil {
		t.Errorf("the run was not logged in the environment env: names: %v", err)
	}
}

// The reported data loss, end to end: 'camp shell' typed inside a running
// session. The refusal has to come before the sweep, and the test can see
// which came first because a sweep leaves a trace -- a work directory a
// finished session left behind, planted here, is what a sweep removes.
// If the refusal is gone or moved below the sweep, that directory goes
// and stderr says "swept:".
//
// The session is a real one, so this needs permission to create a user
// namespace and skips where the binary has none.
func TestEnteringFromInsideIsRefusedBeforeAnythingSweeps(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	built, refused := plan.Prepare(cfg)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}
	if err := compose.Directories(built); err != nil {
		t.Fatal(err)
	}
	if _, problems := gen.Prepare(built); !problems.Empty() {
		t.Fatalf("generation was refused:\n%v", problems)
	}

	// What a finished session leaves: an entry whose live directory is gone.
	stale := filepath.Join(env.Path, config.Dir, "work", "000000000000")
	testenv.MkDir(t, filepath.Join(stale, "work"))
	marker := enc.Document([]string{
		enc.Line("live", filepath.Join(env.Path, "gone")),
		enc.Line("config", cfg.Source),
	})
	if err := os.WriteFile(filepath.Join(stale, compose.MarkerName), marker, 0o644); err != nil {
		t.Fatal(err)
	}

	repo, ok := cfg.Repository(cfg.Upper)
	if !ok {
		t.Fatal("the fixture names no upper")
	}
	pair, err := locks.TakePair(cfg.Env, repo.Path.Components(), cfg.Merged.Components(),
		cfg.UpperPath(), cfg.Live())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pair.Release)

	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := os.Create(filepath.Join(testenv.Root(t), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.Create(filepath.Join(testenv.Root(t), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()

	t.Setenv(insideEnv, cfg.Source)
	status, err := session.Launch(session.Options{
		Config: cfg,
		Argv:   []string{os.Args[0]},
		Locks:  pair,
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
	if err != nil {
		var single refusal.R
		if errors.As(err, &single) && single.Rule == "namespace-denied" ||
			strings.Contains(err.Error(), "unprivileged_userns") ||
			strings.Contains(err.Error(), "operation not permitted") ||
			strings.Contains(err.Error(), "permission denied") {
			t.Skipf("this binary may not create a user namespace, so a session "+
				"cannot be started from a checkout: %v", err)
		}
		t.Fatal(err)
	}
	if status != 0 {
		text, _ := os.ReadFile(stderr.Name())
		t.Fatalf("the workload exited %d:\n%s", status, text)
	}

	output, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	var got shellReport
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatalf("the workload's report did not parse: %v\n%s", err, output)
	}

	if got.Code != cli.ExitBusy || !strings.Contains(got.Stderr, "from inside it") {
		t.Errorf("'camp shell' inside the session exited %d and said:\n%s", got.Code, got.Stderr)
	}
	if strings.Contains(got.Stderr, "swept:") {
		t.Errorf("the sweep ran before the refusal:\n%s", got.Stderr)
	}
	if _, err := os.Stat(filepath.Join(stale, "work")); err != nil {
		t.Errorf("the planted stale work directory is gone, so something swept " +
			"before the refusal")
	}
	if _, err := os.Stat(filepath.Join(built.Work, "work")); err != nil {
		t.Errorf("the running session's own work directory is gone: %v", err)
	}
}
