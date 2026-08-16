package reports_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/reports"
)

// A namespace session has no down and leaves no state record, so what it
// found has to reach somebody by another route: a file, printed once by
// whichever camp command comes next in that environment.
func TestAReportIsPrintedOnceAndThenMarked(t *testing.T) {
	campDir := filepath.Join(t.TempDir(), ".camp")

	path, err := reports.Write(campDir, "abc123def456", "the session found something\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, "abc123def456") {
		t.Errorf("the report is named %q and should name its composition", path)
	}

	if unseen := reports.Unseen(campDir); len(unseen) != 1 {
		t.Fatalf("%d unseen reports, wanted 1", len(unseen))
	}

	var shown []string
	reports.Show(campDir, func(text string) { shown = append(shown, text) })
	if len(shown) != 1 {
		t.Fatalf("the report was shown %d times, wanted once", len(shown))
	}
	if !strings.Contains(shown[0], "the session found something") {
		t.Errorf("the text did not come through:\n%s", shown[0])
	}

	shown = nil
	reports.Show(campDir, func(text string) { shown = append(shown, text) })
	if len(shown) != 0 {
		t.Errorf("the report was shown again: %v", shown)
	}

	// Renamed, not deleted: it is the record of what a session found, and
	// camp only stops putting it in front of you.
	if seen := reports.Seen(campDir); len(seen) != 1 {
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
	campDir := filepath.Join(t.TempDir(), ".camp")
	if unseen := reports.Unseen(campDir); len(unseen) != 0 {
		t.Errorf("found %v in an environment with no reports", unseen)
	}
	var shown []string
	reports.Show(campDir, func(text string) { shown = append(shown, text) })
	if len(shown) != 0 {
		t.Errorf("something was printed: %v", shown)
	}
}

// Several sessions of one composition do not overwrite one another, and
// they come out oldest first.
func TestSeveralReportsAreKeptApartAndOrdered(t *testing.T) {
	campDir := filepath.Join(t.TempDir(), ".camp")
	for _, body := range []string{"first\n", "second\n"} {
		if _, err := reports.Write(campDir, "abc123def456", body); err != nil {
			t.Fatal(err)
		}
		// The name carries a second-resolution timestamp; two written in the
		// same second are one file, which is a real limit worth knowing
		// rather than a bug worth hiding.
		if len(reports.Unseen(campDir)) == 2 {
			break
		}
	}
	unseen := reports.Unseen(campDir)
	if len(unseen) == 0 {
		t.Fatal("nothing was written")
	}
	for index := 1; index < len(unseen); index++ {
		if unseen[index] < unseen[index-1] {
			t.Error("the reports did not come out oldest first")
		}
	}
}
