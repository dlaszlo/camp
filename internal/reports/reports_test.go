package reports_test

import (
	"os"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/reports"
)

// environment is a scratch environment root, held open the way a parsed
// configuration holds one.
func environment(t *testing.T) pathx.Root {
	t.Helper()
	root, err := pathx.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

// A namespace session has no down and leaves no state record, so what it
// found has to reach somebody by another route: a file, printed once by
// whichever camp command comes next in that environment.
func TestAReportIsPrintedOnceAndThenMarked(t *testing.T) {
	env := environment(t)

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
	env := environment(t)
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
	env := environment(t)
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
	env := environment(t)
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

// A mark that cannot be made is said out loud, and the report survives to
// be delivered when it can.
//
// The failure is injected where it can really happen: the reports
// directory is made unwritable, so the rename that marks a report as read
// fails. What must not happen is either silent half of it -- a report
// swallowed because it could not be marked, or a mark claimed that was
// never made and a finding that then arrives at every camp command with
// nothing saying why.
func TestAReportThatCannotBeMarkedIsSaidAndDeliveredOnceItCanBe(t *testing.T) {
	env := environment(t)
	if _, err := reports.Write(env, "abc123def456", "the session found something\n"); err != nil {
		t.Fatal(err)
	}

	directory := reports.Dir(env)
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(directory, 0o755) })

	var shown []string
	collect := func(text string) { shown = append(shown, text) }

	reports.Show(env, collect)
	if len(shown) != 2 {
		t.Fatalf("a report whose mark failed produced %d lines, wanted the "+
			"report and the reason:\n%v", len(shown), shown)
	}
	if !strings.Contains(shown[0], "the session found something") {
		t.Errorf("the finding did not come through:\n%s", shown[0])
	}
	if !strings.Contains(shown[1], "could not be marked") ||
		!strings.Contains(shown[1], "print it again") {
		t.Errorf("nothing said the mark failed and what follows from it:\n%s", shown[1])
	}
	if len(reports.Unseen(env)) != 1 {
		t.Error("the report was dropped by the mark that failed")
	}
	if len(reports.Seen(env)) != 0 {
		t.Error("something was marked as read although the rename failed")
	}

	// The obstruction is gone: the next command delivers it -- the repeat
	// the line above promised -- marks it, and no command after that shows
	// it again.
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	shown = nil
	reports.Show(env, collect)
	if len(shown) != 1 || !strings.Contains(shown[0], "the session found something") {
		t.Fatalf("the report was not delivered once the mark could be made:\n%v", shown)
	}
	shown = nil
	reports.Show(env, collect)
	if len(shown) != 0 {
		t.Errorf("the report was shown again after it was marked: %v", shown)
	}
	if len(reports.Seen(env)) != 1 {
		t.Errorf("%d marked reports, wanted the one", len(reports.Seen(env)))
	}
}

// Marking is a rename in the same directory, so a .seen file that is
// already there is never replaced.
func TestMarkingNeverReplacesAReportSomebodyKept(t *testing.T) {
	env := environment(t)
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
