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

func launch(t *testing.T, argv []string, stdout, stderr *os.File) (int, error) {
	t.Helper()

	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
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
	built = gen.Expand(built, generated)

	pair, err := takeLocks(cfg)
	if err != nil {
		t.Fatalf("the locks could not be taken: %v", err)
	}
	t.Cleanup(pair.Release)

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { devnull.Close() })

	return session.Launch(session.Options{
		Config:  cfg,
		Plan:    built,
		Exclude: generated.Exclude,
		Argv:    argv,
		Locks:   pair,
		Stdin:   devnull,
		Stdout:  stdout,
		Stderr:  stderr,
	})
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

// The launcher waits for the workload, not for the init, and exits with
// the workload's own status.
func TestAForegroundCommandsExitStatusIsPropagated(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()

	status, err := launch(t, []string{"/bin/sh", "-c", "exit 7"}, devnull, os.Stderr)
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
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()

	status, err := launch(t, []string{"/bin/sh", "-c", "kill -TERM $$"}, devnull, os.Stderr)
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
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")

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
	built = gen.Expand(built, generated)

	pair, err := takeLocks(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pair.Release()

	output, err := os.CreateTemp(env.Path, "out-")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()

	status, err := session.Launch(session.Options{
		Config:  cfg,
		Plan:    built,
		Exclude: generated.Exclude,
		Argv:    []string{"/bin/sh", "-c", "ls -a"},
		Locks:   pair,
		Stdin:   devnull,
		Stdout:  output,
		Stderr:  os.Stderr,
	})
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
	entries, err := os.ReadDir(env.Live)
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
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")

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
	built = gen.Expand(built, generated)

	pair, err := takeLocks(cfg)
	if err != nil {
		t.Fatal(err)
	}

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()

	marker := filepath.Join(env.Path, "still-running")
	// setsid detaches it from this process group and it closes its
	// descriptors, exactly as a tmux server does.
	daemon := []string{"/bin/sh", "-c",
		"setsid /bin/sh -c 'sleep 20 >/dev/null 2>&1 </dev/null' & echo started > " + marker}

	started := time.Now()
	status, err := session.Launch(session.Options{
		Config:  cfg,
		Plan:    built,
		Exclude: generated.Exclude,
		Argv:    daemon,
		Locks:   pair,
		Stdin:   devnull,
		Stdout:  devnull,
		Stderr:  os.Stderr,
	})
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
	pair.Release()
	if _, err := takeLocks(cfg); err == nil {
		t.Error("a second composition was accepted while the session was still " +
			"running.\nThe init is camp resident as pid 1 of the namespace and it " +
			"holds the locks: a daemonising workload routinely closes the " +
			"descriptors it inherited, so the design must not depend on the " +
			"workload keeping them")
	}

}
