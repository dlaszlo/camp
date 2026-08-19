package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/logs"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/report"
	"github.com/dlaszlo/camp/internal/testenv"
)

// The launcher's log opens and the init's does not: one warning, and the
// session goes on.
//
// Two processes write one session's log, and most of what a session says
// is said by the second one -- the mounts, the verification, the
// farewell. A failure to open it there used to be stepped over silently,
// which left a file holding the four lines before the interesting ones
// and nothing in it saying why it stops. The session still starts: it
// has a workload to run, and a record not being kept is not a reason to
// refuse one.
func TestTheInitSaysOnceWhenItCannotKeepTheLog(t *testing.T) {
	env, err := pathx.OpenRoot(testenv.Root(t))
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()

	// The launcher's half, which succeeds and writes.
	launcher, err := logs.Open(env, nil)
	if err != nil {
		t.Fatalf("the launcher could not open the log: %v", err)
	}
	if _, err := launcher.Write([]byte("[OK] locks: taken\n")); err != nil {
		t.Fatalf("the launcher could not write to the log: %v", err)
	}
	launcher.Close()

	kept, err := os.ReadFile(logs.Path(env))
	if err != nil || !strings.Contains(string(kept), "locks: taken") {
		t.Fatalf("the launcher's half of the log is not there (%v):\n%s", err, kept)
	}

	// And then the file stops being openable, which is the state the init
	// meets: the log is there, the launcher's lines are in it, and this
	// process cannot append to it.
	file := filepath.Join(env.Name(), ".camp", "logs", "camp.log")
	if err := os.Chmod(file, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(file, 0o644) })
	if _, err := logs.Open(env, nil); err == nil {
		t.Skip("this process can write a file with no permissions, so the " +
			"failure this test is about cannot be arranged here")
	}

	var terminal bytes.Buffer
	sink := report.To(&terminal)
	keepLog(sink, env)

	// It carried on, and everything after it still reaches the person at
	// the terminal.
	report.Narrate(sink).Note("the session goes on")
	sink.Close()

	said := terminal.String()
	if strings.Count(said, "[WARN]") != 1 {
		t.Errorf("the init owes exactly one warning about the log and said "+
			"%d:\n%s", strings.Count(said, "[WARN]"), said)
	}
	if !strings.Contains(said, "not being written") {
		t.Errorf("the init did not say that its half of the log is missing:\n%s", said)
	}
	if !strings.Contains(said, "the session goes on") {
		t.Errorf("the session did not carry on after the log failed:\n%s", said)
	}
}
