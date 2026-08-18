package reports_test

import (
	"os"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/reports"
)

// A namespace session has no down and leaves no state record, so what it
// found has to reach somebody by another route: a file, printed once by
// whichever camp command comes next in that environment.
func TestAReportIsPrintedOnceAndThenMarked(t *testing.T) {
	env := t.TempDir()

	path, err := reports.Write(env, "abc123def456", "the session found something\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, "abc123def456") {
		t.Errorf("the report is named %q and should name its composition", path)
	}

	if unseen := reports.Unseen(env); len(unseen) != 1 {
		t.Fatalf("%d unseen reports, wanted 1", len(unseen))
	}

	var shown []string
	reports.Show(env, func(text string) { shown = append(shown, text) })
	if len(shown) != 1 {
		t.Fatalf("the report was shown %d times, wanted once", len(shown))
	}
	if !strings.Contains(shown[0], "the session found something") {
		t.Errorf("the text did not come through:\n%s", shown[0])
	}

	shown = nil
	reports.Show(env, func(text string) { shown = append(shown, text) })
	if len(shown) != 0 {
		t.Errorf("the report was shown again: %v", shown)
	}

	// Renamed, not deleted: it is the record of what a session found, and
	// camp only stops putting it in front of you.
	if seen := reports.Seen(env); len(seen) != 1 {
		t.Fatalf("%d reports kept, wanted 1", len(seen))
	}
	if _, err := os.Stat(path + reports.SeenSuffix); err != nil {
		t.Errorf("the marked report is not where it should be: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the unmarked report is still there, so it would be shown again")
	}
}

// Nothing to say means no file at all: an environment where every session
// ended cleanly should not accumulate empty reports.
func TestAnEnvironmentWithNoReportsIsQuiet(t *testing.T) {
	env := t.TempDir()
	if unseen := reports.Unseen(env); len(unseen) != 0 {
		t.Errorf("found %v in an environment with no reports", unseen)
	}
	var shown []string
	reports.Show(env, func(text string) { shown = append(shown, text) })
	if len(shown) != 0 {
		t.Errorf("something was printed: %v", shown)
	}
}

// Several sessions of one composition do not overwrite one another, and
// they come out oldest first.
//
// Two written back to back are two files. They used to be one: the name
// carried the time in seconds, and the second report replaced the first
// -- everything a session found, discarded to make room for what the next
// one found.
func TestSeveralReportsAreKeptApartAndOrdered(t *testing.T) {
	env := t.TempDir()
	for _, body := range []string{"first\n", "second\n"} {
		if _, err := reports.Write(env, "abc123def456", body); err != nil {
			t.Fatal(err)
		}
	}
	unseen := reports.Unseen(env)
	if len(unseen) != 2 {
		t.Fatalf("two sessions ending at once left %d reports", len(unseen))
	}
	for index := 1; index < len(unseen); index++ {
		if unseen[index] < unseen[index-1] {
			t.Error("the reports did not come out oldest first")
		}
	}
}

// A report is delivered once, and a delivery that fails says so.
//
// This file is the whole of what a session that has already ended found:
// by the time it is read, its terminal is gone. A read error that was
// skipped silently was that finding lost, and a failed mark meant the
// same text arriving at every camp command afterwards with nothing saying
// why.
func TestADeliveryThatFailsIsSaidRatherThanSkipped(t *testing.T) {
	env := t.TempDir()
	path, err := reports.Write(env, "abc123def456", "the session found something\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) })

	var shown []string
	reports.Show(env, func(text string) { shown = append(shown, text) })
	if len(shown) != 1 || !strings.Contains(shown[0], "could not be read") {
		t.Fatalf("a report that could not be read was passed over: %v", shown)
	}
	// And it is still there to be read once somebody fixes it.
	if len(reports.Unseen(env)) != 1 {
		t.Error("the unreadable report was marked as read anyway")
	}
}

// Marking is a rename in the same directory, so a .seen file that is
// already there is never replaced.
func TestMarkingNeverReplacesAReportSomebodyKept(t *testing.T) {
	env := t.TempDir()
	path, err := reports.Write(env, "abc123def456", "the first session\n")
	if err != nil {
		t.Fatal(err)
	}
	kept := path + reports.SeenSuffix
	if err := os.WriteFile(kept, []byte("an older session, kept on purpose\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := reports.MarkSeen(env, path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(kept)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "older session") {
		t.Error("marking a report as read replaced one that was already there")
	}
	if len(reports.Seen(env)) != 2 {
		t.Errorf("the marked report did not land beside it: %v", reports.Seen(env))
	}
}
