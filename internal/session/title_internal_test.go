package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A directory's name reaches the terminal, so what a directory may be
// called is what has to be defended against.
//
// A name may hold anything but a slash and a null, an escape character
// among them. Written through, whoever can create a directory could write
// to the terminal of whoever composes it: move the cursor, rename the
// window, and on terminals that answer queries, put text where a shell
// will read it as typed. The title is the one place camp writes bytes
// that a terminal interprets rather than prints, so it is the one place
// this can happen.
func TestADirectoryNameCannotWriteToTheTerminal(t *testing.T) {
	for _, probe := range []struct {
		name   string
		given  string
		wanted string
	}{
		{"an ordinary name", "camp-env", "camp-env"},
		{"an escape", "work\x1b]0;owned\x07", "work]0;owned"},
		{"a bell", "work\aowned", "workowned"},
		{"a newline", "work\nowned", "workowned"},
		{"letters this project is written among", "kísérlet", "kísérlet"},
		{"nothing printable at all", "\x1b\a\n", ""},
	} {
		t.Run(probe.name, func(t *testing.T) {
			if got := printable(probe.given); got != probe.wanted {
				t.Errorf("printable(%q) is %q, wanted %q",
					probe.given, got, probe.wanted)
			}
		})
	}

	// And a title bar is not a place to put a path. A name longer than one
	// is cut rather than sent.
	long := printable(strings.Repeat("x", 500))
	if len(long) != 64 {
		t.Errorf("a 500-character name became %d characters, wanted 64", len(long))
	}
}

// Nothing is written to something that is not a terminal.
//
// An escape sequence in a log file or a pipe is not a marker; it is
// corruption of somebody's output, and camp's own output is read by
// tests, by scripts and by whoever redirects it.
func TestNothingIsWrittenToWhatIsNotATerminal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-terminal")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	nameTerminal(file, "/home/somebody/camp-env")()
	nameTerminal(nil, "/home/somebody/camp-env")()

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 0 {
		t.Errorf("%d byte(s) went to something that is not a terminal: %q",
			len(written), written)
	}
}
