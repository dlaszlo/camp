package report

import (
	"os"
	"strings"
	"testing"
)

// Colour is available only where it is honest: a real terminal, no
// NO_COLOR, and a TERM that claims it can show any.
func TestColourIsOnlyOfferedWhereItIsHonest(t *testing.T) {
	if Colour(&strings.Builder{}) {
		t.Error("a stream that is not even a file was offered colour")
	}
	if Colour(devNull(t)) {
		t.Error("a redirected stream was offered colour")
	}

	terminal := pty(t)
	t.Setenv("TERM", "xterm")
	os.Unsetenv("NO_COLOR")
	if !Colour(terminal) {
		t.Fatal("a real terminal was not offered colour")
	}
	t.Setenv("NO_COLOR", "")
	if Colour(terminal) {
		t.Error("NO_COLOR was set and colour was used anyway -- its presence " +
			"is the whole convention, whatever it holds")
	}
	os.Unsetenv("NO_COLOR")
	t.Setenv("TERM", "dumb")
	if Colour(terminal) {
		t.Error("TERM=dumb said it cannot show colour and colour was used anyway")
	}
}

// The marker is coloured and nothing else, so the message stays
// greppable: a sentence with escape codes through it is a sentence
// nothing downstream can match on.
func TestOnlyTheMarkerIsColoured(t *testing.T) {
	sink := &Sink{terminal: &strings.Builder{}, colour: true}

	painted := sink.paint(Marked(MarkError, "camp run failed"))
	if !strings.HasPrefix(painted, sgrBold+MarkError+sgrReset) {
		t.Errorf("[ERROR] is not bold red on its own: %q", painted)
	}
	if strings.Contains(painted[len(sgrBold+MarkError+sgrReset):], "\x1b") {
		t.Errorf("something after the marker is coloured too: %q", painted)
	}
	for marker, want := range map[string]string{
		MarkOK: sgrGreen, MarkNote: sgrBlue, MarkWarn: sgrYellow, MarkHint: sgrYellow,
	} {
		if got := sink.paint(marker + " text"); !strings.HasPrefix(got, want+marker) {
			t.Errorf("%s is not %q: %q", marker, want, got)
		}
	}
	// A line that is not a marked one -- a refusal's continuation, a
	// paragraph a command wrote itself -- is left exactly as it was.
	for _, plain := range []string{"   the second line of a refusal", "[unknown] x", "no marker"} {
		if got := sink.paint(plain); got != plain {
			t.Errorf("%q was painted: %q", plain, got)
		}
	}
}

// Never into the log. The colouring is a property of the sink, which is
// why the file gets the plain words while the terminal gets the marker.
func TestTheLogNeverGetsColour(t *testing.T) {
	var kept strings.Builder
	sink := &Sink{terminal: &strings.Builder{}, colour: true, log: &kept}

	Narrate(sink).Locks("/env/code", "/env/live")
	sink.Close()

	if strings.Contains(kept.String(), "\x1b") {
		t.Errorf("an escape code reached the log: %q", kept.String())
	}
	if !strings.HasPrefix(kept.String(), MarkOK) {
		t.Errorf("the log lost the marker: %q", kept.String())
	}
}

func devNull(t *testing.T) *os.File {
	t.Helper()
	file, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { file.Close() })
	return file
}

// pty is a real terminal to ask the question of.
func pty(t *testing.T) *os.File {
	t.Helper()
	file, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal to test against: %v", err)
	}
	t.Cleanup(func() { file.Close() })
	return file
}
