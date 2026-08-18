package locks_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dlaszlo/camp/internal/locks"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/testenv"
)

// The tests need a second process, because a second flock from the same
// process succeeds -- the lock belongs to the open file description, and
// re-locking one's own is how a lock is upgraded, not a conflict. The
// test binary re-executes itself for that.
const (
	childEnv  = "CAMP_LOCK_CHILD"
	holdEnv   = "CAMP_LOCK_HOLD"
	targetEnv = "CAMP_LOCK_TARGET"
	baseEnv   = "CAMP_LOCK_BASE"
)

func TestMain(m *testing.M) {
	switch os.Getenv(childEnv) {
	case "try":
		os.Exit(childTry())
	case "hold":
		os.Exit(childHold())
	}
	os.Exit(m.Run())
}

// childTry attempts the lock once and reports the answer through its exit
// status: 0 taken, 3 refused as busy, 1 anything else.
func childTry() int {
	base := os.Getenv(baseEnv)
	target := os.Getenv(targetEnv)
	held, err := locks.Take(locks.Upper, base, []string{target}, filepath.Join(base, target))
	if err == nil {
		held.Release()
		return 0
	}
	os.Stderr.WriteString(err.Error() + "\n")
	var single refusal.R
	if errors.As(err, &single) && strings.HasSuffix(single.Rule, "-locked") {
		return 3
	}
	return 1
}

// childHold takes the lock and waits to be killed, so the parent can see
// what happens when a holder dies without releasing anything.
func childHold() int {
	base := os.Getenv(baseEnv)
	target := os.Getenv(targetEnv)
	held, err := locks.Take(locks.Upper, base, []string{target}, filepath.Join(base, target))
	if err != nil {
		return 1
	}
	defer held.Release()
	os.Stdout.WriteString("held\n")
	os.Stdout.Close()
	time.Sleep(2 * time.Minute)
	return 0
}

func try(t *testing.T, base, target string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childEnv+"=try", baseEnv+"="+base, targetEnv+"="+target)
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	t.Fatalf("running the child: %v", err)
	return -1
}

func env(t *testing.T) (string, string) {
	t.Helper()
	root := testenv.Root(t)
	testenv.MkDir(t, filepath.Join(root, "code"))
	testenv.MkDir(t, filepath.Join(root, "live"))
	return root, filepath.Join(root, "code")
}

// The measured double-run scenario: two concurrent runs each composed the
// same upper. Now the second one is refused.
func TestASecondCompositionOnTheSameUpperIsRefused(t *testing.T) {
	root, code := env(t)

	held, err := locks.Take(locks.Upper, root, []string{"code"}, code)
	if err != nil {
		t.Fatalf("the first lock should have been taken: %v", err)
	}
	defer held.Release()

	if code := try(t, root, "code"); code != 3 {
		t.Fatalf("the second attempt exited %d; it should have been refused as busy", code)
	}
}

// The identity is the inode, not a lock file and not a string. A lock
// file under the environment directory meant two environment directories
// naming the same upper locked two different inodes and neither saw the
// other.
func TestTheSameDirectoryReachedByAnotherPathIsTheSameLock(t *testing.T) {
	root, code := env(t)
	alias := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(alias) })

	held, err := locks.Take(locks.Upper, root, []string{"code"}, code)
	if err != nil {
		t.Fatalf("the first lock should have been taken: %v", err)
	}
	defer held.Release()

	if code := try(t, alias, "code"); code != 3 {
		t.Fatalf("a lock taken through a second path to the same directory "+
			"exited %d; every path to one directory has to be one lock", code)
	}
}

func TestTwoDifferentDirectoriesLockIndependently(t *testing.T) {
	root, code := env(t)

	held, err := locks.Take(locks.Upper, root, []string{"code"}, code)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	if code := try(t, root, "live"); code != 0 {
		t.Fatalf("locking a different directory exited %d; several compositions "+
			"with different uppers and different live paths stay fine", code)
	}
}

// A second composition on the same live directory is refused too, and the
// two locks are taken upper first, live second, so racing camps can only
// refuse each other rather than deadlock.
func TestBothLocksAreHeldAtOnceAndTheSecondFailureReleasesTheFirst(t *testing.T) {
	root, code := env(t)
	live := filepath.Join(root, "live")

	pair, err := locks.TakePair(root, []string{"code"}, []string{"live"}, code, live)
	if err != nil {
		t.Fatalf("one process should be able to hold both: %v", err)
	}

	if got := try(t, root, "live"); got != 3 {
		t.Fatalf("a second composition on the same live directory exited %d", got)
	}
	pair.Release()

	// After release both are free again.
	if got := try(t, root, "code"); got != 0 {
		t.Errorf("the upper stayed locked after release (exit %d)", got)
	}
	if got := try(t, root, "live"); got != 0 {
		t.Errorf("the live directory stayed locked after release (exit %d)", got)
	}
}

// A refusal leaves nothing held. The upper is taken first, so a live
// directory that is already busy has to give the upper back.
func TestTheSecondLockFailingReleasesTheFirst(t *testing.T) {
	root, code := env(t)
	live := filepath.Join(root, "live")

	blocker := hold(t, root, "live")

	if _, err := locks.TakePair(root, []string{"code"}, []string{"live"}, code, live); err == nil {
		t.Fatal("the pair was taken although the live directory was busy")
	}

	// The upper must be free again even though this process took it first.
	if got := try(t, root, "code"); got != 0 {
		t.Errorf("the upper lock survived a failed pair (exit %d); a refusal has "+
			"to leave nothing held", got)
	}

	_ = blocker.Process.Signal(syscall.SIGKILL)
	_ = blocker.Wait()
}

// hold starts a child that takes one lock and waits, and waits until it
// reports that it has it.
func hold(t *testing.T, base, target string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childEnv+"=hold", baseEnv+"="+base, targetEnv+"="+target)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_ = cmd.Wait()
	})
	buffer := make([]byte, 5)
	if _, err := out.Read(buffer); err != nil {
		t.Fatalf("the holder never reported holding %s: %v", target, err)
	}
	return cmd
}

// The kernel releases the lock when the holder dies. That is the whole
// reason the guard is something held rather than something written: a
// record can go stale after kill -9 and would then need exactly the
// --force this design refuses.
func TestTheLockIsReleasedWhenTheHolderIsKilled(t *testing.T) {
	root, _ := env(t)

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childEnv+"=hold", baseEnv+"="+root, targetEnv+"=code")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 5)
	if _, err := out.Read(buffer); err != nil {
		t.Fatalf("the holder never reported holding the lock: %v", err)
	}

	if got := try(t, root, "code"); got != 3 {
		t.Fatalf("while the holder lives the lock should be refused (exit %d)", got)
	}

	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	if got := try(t, root, "code"); got != 0 {
		t.Fatalf("after kill -9 the lock should be gone (exit %d); there is no "+
			"stale lock to clear and no --force to reach for", got)
	}
}

// Locking the code repository must not touch it: camp never modifies a
// repository, and an flock that left a trace would break that invariant
// on the very directory it is protecting.
func TestLockingWritesNothingIntoTheDirectory(t *testing.T) {
	root, code := env(t)
	testenv.Write(t, filepath.Join(code, "file"), "x\n")

	before, err := os.Stat(code)
	if err != nil {
		t.Fatal(err)
	}
	beforeEntries, err := os.ReadDir(code)
	if err != nil {
		t.Fatal(err)
	}

	held, err := locks.Take(locks.Upper, root, []string{"code"}, code)
	if err != nil {
		t.Fatal(err)
	}
	held.Release()

	after, err := os.Stat(code)
	if err != nil {
		t.Fatal(err)
	}
	afterEntries, err := os.ReadDir(code)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("the directory's mtime changed; locking has to leave no trace")
	}
	if len(beforeEntries) != len(afterEntries) {
		t.Error("an entry appeared in the directory; there is no lock file anywhere")
	}
}

// The refusal has to name what holds the directory, read from /proc and
// never from any program's output.
func TestTheRefusalNamesTheHolder(t *testing.T) {
	root, code := env(t)

	held, err := locks.Take(locks.Upper, root, []string{"code"}, code)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	holders := locks.Holders(code)
	if len(holders) == 0 {
		t.Fatal("no holder was found for a directory this process is holding")
	}
	found := false
	for _, holder := range holders {
		if holder.PID == os.Getpid() {
			found = true
		}
	}
	if !found {
		t.Errorf("the holders found were %v, and this process (%d) is not among them",
			holders, os.Getpid())
	}

	// And the message a second camp would print.
	_, err = locks.Take(locks.Upper, root, []string{"code"}, code)
	if err == nil {
		t.Skip("this process can re-lock its own description; the message is " +
			"exercised by the child tests")
	}
}

func TestTheBusyMessageSaysHowToGetIn(t *testing.T) {
	root, code := env(t)
	held, err := locks.Take(locks.Upper, root, []string{"code"}, code)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childEnv+"=try", baseEnv+"="+root, targetEnv+"=code")
	output, _ := cmd.CombinedOutput()
	message := string(output)

	for _, want := range []string{code, "tmux", "second one", "--force"} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal does not mention %q:\n\n%s", want, message)
		}
	}
}

// The refusal has to name the process that is holding the lock *now*,
// not the one that took it.
//
// Those are different processes on purpose: the launcher takes the locks
// and hands the open file descriptions to the session's init, which is
// what makes the locks last exactly as long as the composition. By the
// time anybody asks, the pid recorded in /proc/locks has usually exited
// -- and a message that names a dead pid and then says "unknown" is
// exactly the message this is not allowed to be.
func TestTheHolderIsTheProcessThatHasItNowNotTheOneThatTookIt(t *testing.T) {
	root, code := env(t)

	// A child that holds the lock and lives, standing in for the init.
	holder := hold(t, root, "code")

	found := locks.Holders(code)
	if len(found) == 0 {
		t.Fatal("no holder was found for a directory a live process is holding")
	}
	var named bool
	for _, candidate := range found {
		if candidate.PID != holder.Process.Pid {
			continue
		}
		named = true
		if candidate.Command == "unknown" || candidate.Command == "" {
			t.Errorf("the holder was found but not named: %q", candidate.Command)
		}
	}
	if !named {
		t.Errorf("the process actually holding the lock (pid %d) is not among "+
			"the holders found: %v", holder.Process.Pid, found)
	}
}

// The steady-state guard compares directories, not the strings that name
// them.
//
// Between an up and a down no camp process is alive, so the flocks are
// gone and the mount table is the only thing left that knows. A second
// composition naming the same upper through a bind mount somewhere else
// spells it differently, and a scan matching the upperdir string would
// let it through -- two overlays on one upper, which the kernel permits
// and which corrupts both.
func TestTheUpperScanComparesDirectoriesAndNotNames(t *testing.T) {
	root := t.TempDir()
	upper := filepath.Join(root, "code")
	if err := os.MkdirAll(upper, 0o755); err != nil {
		t.Fatal(err)
	}

	// The same directory, named another way: the path goes through a link
	// to the environment root, the way a bind mount of the repository
	// elsewhere would name it. Both spellings resolve to one inode, which
	// is the only thing the scan may believe.
	if err := os.Symlink(root, filepath.Join(root, "elsewhere-root")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "elsewhere-root", "code")

	line := "60 1 0:70 / " + filepath.Join(root, "live") +
		" rw,relatime - overlay overlay rw,lowerdir=" + filepath.Join(root, "workspace") +
		",upperdir=" + alias + ",workdir=" + filepath.Join(root, "work")
	table, err := mountinfo.Read(writeTable(t, root, line))
	if err != nil {
		t.Fatal(err)
	}

	refused := locks.ScanUpper(table, upper)
	if !refused.Has("upper-already-composed") {
		t.Fatalf("an overlay on the same directory under another name was not "+
			"seen: %v", refused.Rules())
	}
	if !strings.Contains(refused.Error(), "same directory") {
		t.Errorf("the refusal does not say the two names are one directory:\n%v",
			refused.Error())
	}

	// And an overlay on somebody else's upper is still not ours.
	other := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	line = strings.Replace(line, "upperdir="+alias, "upperdir="+other, 1)
	table, err = mountinfo.Read(writeTable(t, root, line))
	if err != nil {
		t.Fatal(err)
	}
	if problems := locks.ScanUpper(table, upper); !problems.Empty() {
		t.Errorf("an unrelated overlay was taken for this composition's: %v",
			problems.Rules())
	}
}

func writeTable(t *testing.T, root, line string) string {
	t.Helper()
	path := filepath.Join(root, "mountinfo")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
