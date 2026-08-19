package reports

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dlaszlo/camp/internal/pathx"
)

// A report is delivered once. Not once per command, and not once per
// process: the file is the whole of what a session that has already ended
// found, and by the time anybody reads it there is no session left to ask.
//
// Two camp commands starting at the same moment is the ordinary case --
// a shell prompt hook and a person typing -- so the schedule below is two
// processes, coordinated through files, with the seam that lets the first
// one stop in the window where both used to print.

const (
	showerEnv = "CAMP_TEST_SHOWER"
	syncEnv   = "CAMP_TEST_SYNC"
	rootEnv   = "CAMP_TEST_SHOWER_ROOT"
	body      = "the session found something"
	hash      = "abc123def456"
)

// TestShowHelper is one of the two commands. It runs only when the parent
// below starts it.
func TestShowHelper(t *testing.T) {
	label := os.Getenv(showerEnv)
	if label == "" {
		t.Skip("started only by TestTwoProcessesDeliverOneReportOnce")
	}
	meeting := os.Getenv(syncEnv)

	root, err := pathx.OpenRoot(os.Getenv(rootEnv))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	afterRead = func(string) {
		announce(t, meeting, "read-"+label, "")
		if label == "first" {
			await(t, meeting, "go-first", time.Minute)
		}
	}

	var said []string
	announce(t, meeting, "started-"+label, "")
	Show(root, func(text string) { said = append(said, text) })
	announce(t, meeting, "out-"+label, strings.Join(said, "\n"))
}

// The first command stops between reading the report and printing it, and
// the second runs the whole delivery in that window.
//
// Before the lock, both listed an unmarked report, both read it and both
// printed it, and both renames then reported success because a rename
// whose source was already gone was folded into one. What the reader saw
// was one session's findings twice, from two commands each convinced it
// had done the once-only thing.
func TestTwoProcessesDeliverOneReportOnce(t *testing.T) {
	env := t.TempDir()
	meeting := t.TempDir()

	root, err := pathx.OpenRoot(env)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := Write(root, hash, body+"\n"); err != nil {
		t.Fatal(err)
	}

	first := command(t, "first", env, meeting)
	await(t, meeting, "read-first", 30*time.Second)

	second := command(t, "second", env, meeting)
	await(t, meeting, "started-second", 30*time.Second)

	// Long enough for the second command to list, read and print, which is
	// what it did before the lock and what it must not do now. It costs
	// this long only when the repair holds; when it does not, the file it
	// waits for appears at once and the assertion below is what fails.
	waitFor(meeting, "read-second", 500*time.Millisecond)

	announce(t, meeting, "go-first", "")
	await(t, meeting, "out-first", 30*time.Second)
	await(t, meeting, "out-second", 30*time.Second)
	if err := first.Wait(); err != nil {
		t.Fatalf("the first command: %v", err)
	}
	if err := second.Wait(); err != nil {
		t.Fatalf("the second command: %v", err)
	}

	delivered := 0
	for _, label := range []string{"first", "second"} {
		if strings.Contains(said(t, meeting, "out-"+label), body) {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("%d of the two commands printed the report, wanted one", delivered)
	}
	if len(Unseen(root)) != 0 {
		t.Errorf("the report is still unseen after being delivered")
	}
	if len(Seen(root)) != 1 {
		t.Errorf("%d marked reports, wanted the one", len(Seen(root)))
	}
}

// The command that loses the mark-seen race is told it lost.
//
// Sequentially here, because the losing side's whole view of the race is
// a source that is no longer there, and that is what this arranges. It
// used to come back as success: fsx folded a missing rename source into
// nil for everybody, so both sides of a once-only transition believed
// they had performed it.
func TestTheLoserOfAMarkSeenRaceIsToldItLost(t *testing.T) {
	env := t.TempDir()
	root, err := pathx.OpenRoot(env)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	path, err := Write(root, hash, body+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkSeen(root, path); err != nil {
		t.Fatalf("the first mark: %v", err)
	}
	if err := MarkSeen(root, path); !errors.Is(err, ErrAlreadySeen) {
		t.Fatalf("the second mark returned %v, wanted a state change the "+
			"caller can act on", err)
	}
	// And the report somebody else marked is not marked twice.
	if len(Seen(root)) != 1 {
		t.Errorf("%d marked reports, wanted the one", len(Seen(root)))
	}
}

// What a session killed mid-write leaves behind is not a report.
//
// The atomic write builds its bytes under a dot name in this directory,
// so a crash leaves one there for good. Listed as a report it was printed
// as a session's findings -- a fragment of them, or nothing at all -- and
// then marked as read, which is the one thing that cannot be undone.
func TestAHalfWrittenReportIsNeverDelivered(t *testing.T) {
	env := t.TempDir()
	root, err := pathx.OpenRoot(env)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if _, err := Write(root, hash, body+"\n"); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(Dir(root), "."+hash+"-1755561600000000000.0123456789abcdef.camp")
	if err := os.WriteFile(leftover, []byte("half of a session's findings"), 0o644); err != nil {
		t.Fatal(err)
	}

	if unseen := Unseen(root); len(unseen) != 1 {
		t.Fatalf("%d unseen reports, wanted the one that was written: %v", len(unseen), unseen)
	}
	var said []string
	Show(root, func(text string) { said = append(said, text) })
	if len(said) != 1 || !strings.Contains(said[0], body) {
		t.Fatalf("the delivery said %v", said)
	}
	// Still there, and still not a report: nothing renamed it to .seen.
	if _, err := os.Stat(leftover); err != nil {
		t.Errorf("the leftover was taken for a report and marked: %v", err)
	}
	if len(Seen(root)) != 1 {
		t.Errorf("%d marked reports, wanted the one", len(Seen(root)))
	}
}

// -- the two processes ------------------------------------------------------

func command(t *testing.T, label, env, meeting string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestShowHelper$")
	cmd.Env = append(os.Environ(),
		showerEnv+"="+label,
		syncEnv+"="+meeting,
		rootEnv+"="+env)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

// announce and await are the whole coordination: a name appearing in a
// directory both processes can see. No pipe, because either side may be
// blocked on a lock this test is about and a reader that has to be
// serviced would add a schedule of its own.
func announce(t *testing.T, meeting, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(meeting, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func await(t *testing.T, meeting, name string, patience time.Duration) {
	t.Helper()
	if !waitFor(meeting, name, patience) {
		t.Fatalf("%s never appeared in %s", name, meeting)
	}
}

func waitFor(meeting, name string, patience time.Duration) bool {
	deadline := time.Now().Add(patience)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(meeting, name)); err == nil {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func said(t *testing.T, meeting, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(meeting, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
