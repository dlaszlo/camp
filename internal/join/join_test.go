package join_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dlaszlo/camp/internal/cli"
	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/join"
	"github.com/dlaszlo/camp/internal/locks"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/session"
	"github.com/dlaszlo/camp/internal/testenv"
)

// A join enters a running session through nsenter, which execs this same
// binary as the joined process. So the test binary answers everything
// cmd/camp answers -- the session init's hidden argument, the joined
// process's -- and, when asked to, stands in for the camp command itself,
// so the tests drive 'camp run --join' and 'camp shell --join' as a person
// would rather than the pieces behind them.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == session.InitArg {
		session.InitMain(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == session.JoinedArg {
		session.JoinedMain(os.Args[2:])
		return
	}
	// A joined process asked to probe from inside: it runs Find for the
	// configuration it is standing in and prints the rule that fired, so a
	// test can prove that a join from inside the very session is refused.
	if len(os.Args) > 2 && os.Args[1] == probeArg {
		cfg, err := config.Load(os.Args[2])
		if err != nil {
			os.Stdout.WriteString("load-error\n")
			os.Exit(0)
		}
		_, refused := join.Find(cfg)
		if refused.Empty() {
			os.Stdout.WriteString("no-refusal\n")
		} else {
			os.Stdout.WriteString(refused.Rules()[0] + "\n")
		}
		os.Exit(0)
	}
	if os.Getenv(asCamp) == "1" {
		os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// probeArg makes a joined test binary run Find from inside; asCamp makes
// a test binary be the camp command.
const (
	probeArg = "__probe"
	asCamp   = "CAMP_JOIN_TEST_AS_CAMP"
)

// running is a session started in the background, with the way to end it
// and to read what it exited with.
type running struct {
	cfg  config.Config
	env  *testenv.Env
	go_  string
	done chan int
}

// startSession starts a real session whose workload waits for a file, so a
// test can join it and then release it. It skips where the machine refuses
// the namespace, the way the session tests do.
func startSession(t *testing.T) *running {
	t.Helper()

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
	pair, err := takeLocks(cfg)
	if err != nil {
		t.Fatalf("the locks could not be taken: %v", err)
	}

	ready := filepath.Join(env.Path, "workload-ready")
	goFile := filepath.Join(env.Path, "go")
	workload := []string{"/bin/sh", "-c",
		"echo > " + ready + "; until [ -e " + goFile + " ]; do sleep 0.05; done"}

	quiet, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { quiet.Close() })

	r := &running{cfg: cfg, env: env, go_: goFile, done: make(chan int, 1)}
	launched := make(chan error, 1)
	go func() {
		status, err := session.Launch(session.Options{
			Config: cfg, Locks: pair, Argv: workload,
			Stdin: quiet, Stdout: quiet, Stderr: os.Stderr,
		})
		launched <- err
		r.done <- status
	}()

	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return r
		}
		select {
		case err := <-launched:
			skipUnlessNamespaced(t, err)
			pair.Release()
			t.Fatalf("the session ended before its workload ran: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the session's workload had not run after 20s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func takeLocks(cfg config.Config) (*locks.Pair, error) {
	repo, ok := cfg.Repository(cfg.Upper)
	if !ok {
		return nil, errors.New("the fixture names no upper")
	}
	return locks.TakePair(cfg.Env, repo.Path.Components(), cfg.Merged.Components(),
		cfg.UpperPath(), cfg.Live())
}

// stop releases the workload and waits for the session to end.
func (r *running) stop(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(r.go_, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.done:
	case <-time.After(session.Grace + 10*time.Second):
		t.Fatal("the session did not end after its workload was released")
	}
}

func skipUnlessNamespaced(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	var single refusal.R
	if errors.As(err, &single) && single.Rule == "namespace-denied" {
		t.Skipf("this binary may not create a user namespace, so a session " +
			"cannot be started from a checkout. Install camp and its profile.")
	}
	if strings.Contains(err.Error(), "unprivileged_userns") ||
		strings.Contains(err.Error(), "operation not permitted") ||
		strings.Contains(err.Error(), "permission denied") {
		t.Skipf("the namespace could not be built here: %v", err)
	}
}

// nsenterOrSkip skips a test that needs nsenter on a machine without it.
func nsenterOrSkip(t *testing.T) {
	t.Helper()
	if _, _, ok := join.Nsenter(); !ok {
		t.Skip("nsenter is not installed, so a join cannot be measured here")
	}
}

// campCommand is this test binary standing in for camp, with the given
// arguments, its streams captured into one file.
func campCommand(t *testing.T, cfg config.Config, args ...string) (*exec.Cmd, *os.File) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.CreateTemp(cfg.Env, "camp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { out.Close() })
	cmd := exec.Command(self, args...)
	cmd.Env = append(os.Environ(), asCamp+"=1")
	cmd.Stdout, cmd.Stderr = out, out
	return cmd, out
}

// status reads a finished command's exit the way a shell reports it.
func status(cmd *exec.Cmd) int {
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return cmd.ProcessState.ExitCode()
}

// runJoin drives 'camp run --join -f <config> -- argv' and returns what it
// wrote and how it exited.
func runJoin(t *testing.T, cfg config.Config, argv ...string) (string, int) {
	t.Helper()
	args := append([]string{"run", "--join", "-f", cfg.Source, "--"}, argv...)
	cmd, out := campCommand(t, cfg, args...)
	_ = cmd.Run()
	data, _ := os.ReadFile(out.Name())
	return string(data), status(cmd)
}

// The join, end to end through the command: the joined process is really
// inside the session's pid namespace -- /proc/1 is the init and its own
// NSpid is small -- it stands in the composed tree and writes through it,
// and its exit does not end the session.
func TestAJoinedProcessIsInsideThePidNamespaceAndItsExitDoesNotEndTheSession(t *testing.T) {
	nsenterOrSkip(t)
	r := startSession(t)
	defer r.stop(t)

	target, refused := join.Find(r.cfg)
	if !refused.Empty() {
		t.Fatalf("the running session was not found:\n%v", refused.Error())
	}
	target.Close()
	if target.Live != r.cfg.Live() {
		t.Errorf("the target composes at %s, wanted %s", target.Live, r.cfg.Live())
	}

	script := "printf 'init=%s\\n' \"$(tr '\\0' ' ' < /proc/1/cmdline)\"\n" +
		"printf 'nspid=%s\\n' \"$(awk '/^NSpid:/{print $NF}' /proc/self/status)\"\n" +
		"printf 'pwd=%s\\n' \"$(pwd)\"\n" +
		"for fd in /proc/self/fd/*; do printf 'fd=%s\\n' \"$(readlink \"$fd\")\"; done\n" +
		"echo joined > joined-was-here && printf 'wrote=ok\\n'"
	text, code := runJoin(t, r.cfg, "/bin/sh", "-c", script)
	if code != 0 {
		t.Fatalf("'camp run --join' exited %d:\n%s", code, text)
	}

	if !strings.Contains(text, session.InitArg) {
		t.Errorf("/proc/1 inside the join is not the session init:\n%s", text)
	}
	if !strings.Contains(text, "nspid=") || strings.Contains(text, "nspid=1\n") {
		t.Errorf("the joined process is not a non-init member of the pid "+
			"namespace:\n%s", text)
	}
	if !strings.Contains(text, "pwd="+r.cfg.Live()) {
		t.Errorf("the joined process does not stand in the composed tree %s:\n%s",
			r.cfg.Live(), text)
	}
	if !strings.Contains(text, "wrote=ok") {
		t.Errorf("the joined process could not write through the tree:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(r.cfg.UpperPath(), "joined-was-here")); err != nil {
		t.Errorf("the joined write did not reach the code repository: %v", err)
	}
	// The namespace files nsenter was handed do not reach the shell.
	for _, kind := range []string{"fd=mnt:[", "fd=user:[", "fd=pid:["} {
		if strings.Contains(text, kind) {
			t.Errorf("the joined shell holds a namespace descriptor (%s):\n%s", kind, text)
		}
	}

	// The joined command has exited. The session must still be running: a
	// visitor's exit is not the session's end.
	after, refused := join.Find(r.cfg)
	if !refused.Empty() {
		t.Fatalf("the session ended when a joined process exited, which it must "+
			"not:\n%v", refused.Error())
	}
	after.Close()
}

// 'camp shell --join' is the other door to the same room: the effective
// SHELL, fed a script on stdin, runs inside the session and its status
// comes back as the shell's own.
func TestAJoinedShellRunsInsideAndReportsItsOwnStatus(t *testing.T) {
	nsenterOrSkip(t)
	r := startSession(t)
	defer r.stop(t)

	cmd, out := campCommand(t, r.cfg, "shell", "--join", "-f", r.cfg.Source)
	cmd.Env = append(cmd.Env, "SHELL=/bin/sh")
	cmd.Stdin = strings.NewReader("tr '\\0' ' ' < /proc/1/cmdline; echo; pwd; exit 4\n")
	_ = cmd.Run()
	data, _ := os.ReadFile(out.Name())
	text := string(data)
	if status(cmd) != 4 {
		t.Errorf("'camp shell --join' exited %d, wanted the shell's own 4:\n%s", status(cmd), text)
	}
	if !strings.Contains(text, session.InitArg) || !strings.Contains(text, r.cfg.Live()) {
		t.Errorf("the joined shell was not inside the session at %s:\n%s", r.cfg.Live(), text)
	}
}

// The other direction: when the workload ends, the session's fan-out
// reaches a joined process too, and one that outlives the init is ended by
// the kernel at the init's exit -- nsenter reports it as a signalled exit,
// and the command says the session ended. This is what needs the two-fact
// "empty" the end rule ships, since a joined process is never the init's
// child.
func TestTheWorkloadsEndReachesAJoinedProcess(t *testing.T) {
	nsenterOrSkip(t)
	r := startSession(t)

	jready := filepath.Join(r.env.Path, "joined-ready")
	script := "trap '' TERM; echo > " + jready + "; exec sleep 300"
	type result struct {
		text string
		code int
	}
	done := make(chan result, 1)
	go func() {
		text, code := runJoin(t, r.cfg, "/bin/sh", "-c", script)
		done <- result{text, code}
	}()
	waitFor(t, jready, done)

	if err := os.WriteFile(r.go_, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-done:
		if outcome.code != 128+int(syscall.SIGKILL) {
			t.Errorf("the joined process exited %d; the workload's end should have "+
				"reached it and, since it ignored the request, the kernel should have "+
				"ended it with SIGKILL (%d):\n%s", outcome.code, 128+int(syscall.SIGKILL), outcome.text)
		}
		if !strings.Contains(outcome.text, "the session ended; this shell went with it") {
			t.Errorf("the command did not say the session ended:\n%s", outcome.text)
		}
	case <-time.After(session.Grace + 15*time.Second):
		t.Fatal("the joined process outlived the session; the kernel's pid-1 rule " +
			"did not end it")
	}
	<-r.done
}

// waitFor waits for a file a joined process writes when it is in place, or
// fails on the join ending first.
func waitFor[T any](t *testing.T, path string, done chan T) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case outcome := <-done:
			t.Fatalf("the joined process ended before it was in place: %+v", outcome)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the joined process had not run after 20s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The joiner pins nothing. Its copies of the namespace descriptors close
// the moment nsenter has them, so a joiner that is stopped when the
// session ends holds no reference to the old mount namespace -- and the
// namespace, with the overlay in it, is gone before the freed upper lock
// can be taken again. An open descriptor to a mount namespace would keep
// it alive with no process in it, and a second overlay on the same upper is
// the corruption the locks exist to prevent.
func TestAStoppedJoinerHoldsNoNamespaceAfterTheSessionEnds(t *testing.T) {
	nsenterOrSkip(t)
	r := startSession(t)

	target, refused := join.Find(r.cfg)
	if !refused.Empty() {
		t.Fatalf("the running session was not found:\n%v", refused.Error())
	}
	mountNS, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/mnt", target.PID))
	if err != nil {
		t.Fatal(err)
	}
	target.Close() // this test must not be the one holding it

	jready := filepath.Join(r.env.Path, "joined-ready")
	cmd, out := campCommand(t, r.cfg, "run", "--join", "-f", r.cfg.Source, "--",
		"/bin/sh", "-c", "echo > "+jready+"; exec sleep 300")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	waitFor(t, jready, finished)

	// While the join runs, the joiner itself holds no namespace descriptor.
	for _, link := range descriptors(t, cmd.Process.Pid) {
		for _, kind := range []string{"mnt:[", "user:[", "pid:["} {
			if strings.HasPrefix(link, kind) {
				t.Errorf("the joiner holds a namespace descriptor while the join runs: %s", link)
			}
		}
	}

	// Stop the joiner, end the session, and look for the namespace.
	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	r.stop(t)
	if pair, err := takeLocks(r.cfg); err != nil {
		t.Errorf("the locks are still held after the session ended: %v", err)
	} else {
		pair.Release()
	}
	if holders := referencesTo(mountNS); len(holders) > 0 {
		t.Errorf("the session's mount namespace %s is still referenced after the "+
			"session ended, with the joiner stopped: %v. A new start could mount a "+
			"second overlay on the same upper while this one stands.", mountNS, holders)
	}

	// The joined sleep did not ignore the request, so it died of the
	// fan-out's SIGTERM; nsenter reported that, and the continued joiner
	// passes it on as the shell would.
	_ = syscall.Kill(cmd.Process.Pid, syscall.SIGCONT)
	<-finished
	data, _ := os.ReadFile(out.Name())
	if status(cmd) != 128+int(syscall.SIGTERM) {
		t.Errorf("the joiner exited %d, wanted the joined process's SIGTERM (%d):\n%s",
			status(cmd), 128+int(syscall.SIGTERM), data)
	}
}

// descriptors reads what a process's open descriptors point at.
func descriptors(t *testing.T, pid int) []string {
	t.Helper()
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		t.Fatalf("reading the joiner's descriptors: %v", err)
	}
	var links []string
	for _, entry := range entries {
		if link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, entry.Name())); err == nil {
			links = append(links, link)
		}
	}
	return links
}

// referencesTo lists every visible process that stands in, or holds a
// descriptor to, the named namespace. A namespace nobody references is
// gone.
func referencesTo(ns string) []string {
	var found []string
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		pid := entry.Name()
		if _, err := fmt.Sscanf(pid, "%d", new(int)); err != nil {
			continue
		}
		if link, err := os.Readlink("/proc/" + pid + "/ns/mnt"); err == nil && link == ns {
			found = append(found, "pid "+pid+" is in it")
		}
		fds, err := os.ReadDir("/proc/" + pid + "/fd")
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if link, err := os.Readlink("/proc/" + pid + "/fd/" + fd.Name()); err == nil && link == ns {
				found = append(found, "pid "+pid+" holds fd "+fd.Name())
			}
		}
	}
	return found
}

// A join from inside the very session it names is refused: pid 1 there is
// that configuration's own init, so the shell is already in it. Measured by
// joining the session and, from inside, running Find for the same
// configuration.
func TestAJoinFromInsideTheSameSessionIsRefused(t *testing.T) {
	nsenterOrSkip(t)
	r := startSession(t)
	defer r.stop(t)

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	text, code := runJoin(t, r.cfg, self, probeArg, r.cfg.Source)
	if code != 0 {
		t.Fatalf("the from-inside probe exited %d:\n%s", code, text)
	}
	if !strings.Contains(text, "join-from-inside") {
		t.Errorf("a join from inside the same session was not refused with "+
			"join-from-inside:\n%s", text)
	}
}

// Without a running session, a join refuses and builds nothing. From
// inside another session (which is where the suite itself runs) the refusal
// names that as the reason; from truly outside it is join-no-session. Both
// name the exact command that starts one, with the file, and neither
// returns a target.
func TestJoinRefusesWithoutARunningSession(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")

	target, refused := join.Find(cfg)
	if target != nil {
		target.Close()
		t.Fatal("a join found a session where none is running")
	}
	if refused.Empty() {
		t.Fatal("a join with no session running refused nothing")
	}
	rule := refused.Rules()[0]
	if rule != "join-no-session" && rule != "join-from-another-session" {
		t.Errorf("the refusal is %q, wanted no-session or from-another-session:\n%s",
			rule, refused.Error())
	}
	if !strings.Contains(refused.Error(), "camp shell -f '"+cfg.Source+"'") {
		t.Errorf("the refusal does not give the exact command, with the file:\n%s",
			refused.Error())
	}

	// And through the command, with its exit code.
	cmd, out := campCommand(t, cfg, "shell", "--join", "-f", cfg.Source)
	_ = cmd.Run()
	data, _ := os.ReadFile(out.Name())
	if code := status(cmd); code != cli.ExitNotFound && code != cli.ExitBusy {
		t.Errorf("'camp shell --join' exited %d with no session, wanted %d or %d:\n%s",
			code, cli.ExitNotFound, cli.ExitBusy, data)
	}
}
