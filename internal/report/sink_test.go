package report_test

import (
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/report"
)

// Everything a command says reaches both ends, in the same words. The log
// is not a second format: what is worth having a week later is what the
// person at the terminal saw.
func TestEverySaidLineReachesBothEnds(t *testing.T) {
	var terminal, kept strings.Builder
	sink := report.To(&terminal)
	sink.Keep(&kept)

	say := report.Narrate(sink)
	say.Locks("/env/code", "/env/live")
	say.Warn("a workspace root entry has disappeared")
	if err := sink.Close(); err != nil {
		t.Fatalf("closing the sink: %v", err)
	}

	if terminal.String() != kept.String() {
		t.Errorf("the two ends were told different things:\n%q\n%q",
			terminal.String(), kept.String())
	}
	if !strings.Contains(kept.String(), "locks: /env/code") {
		t.Errorf("the log did not get the line:\n%s", kept.String())
	}
}

// Nothing is kept before a command has found its configuration: until
// then camp does not know whose log it would be.
func TestNothingIsKeptBeforeThereIsSomewhereToKeepIt(t *testing.T) {
	var terminal, kept strings.Builder
	sink := report.To(&terminal)

	report.Narrate(sink).Note("before the configuration was found")
	sink.Keep(&kept)
	report.Narrate(sink).Note("after it was found")
	sink.Close()

	if strings.Contains(kept.String(), "before") {
		t.Errorf("a line was kept before the log existed:\n%s", kept.String())
	}
	if !strings.Contains(kept.String(), "after") {
		t.Errorf("a line after the log was attached was not kept:\n%s", kept.String())
	}
	if !strings.Contains(terminal.String(), "before") {
		t.Errorf("the reader did not see the line either:\n%s", terminal.String())
	}
}

// A write that arrives in pieces is one line, and a last line with no
// newline is still said.
func TestALineArrivingInPiecesIsOneLine(t *testing.T) {
	var terminal, kept strings.Builder
	sink := report.To(&terminal)
	sink.Keep(&kept)

	sink.Write([]byte("[OK] loc"))
	sink.Write([]byte("ks: taken\n[NOTE] and a tail with no newline"))
	sink.Close()

	lines := strings.Split(strings.TrimSuffix(kept.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("the pieces became %d lines:\n%q", len(lines), kept.String())
	}
	if lines[0] != "[OK] locks: taken" {
		t.Errorf("the first line was cut: %q", lines[0])
	}
	if lines[1] != "[NOTE] and a tail with no newline" {
		t.Errorf("the tail was lost or mangled: %q", lines[1])
	}
}
