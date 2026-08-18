package report

import (
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Colour, where it is honestly available, and on the marker alone.
//
// Only the marker, so the message stays calm and greppable: a sentence
// with escape codes through it is a sentence nothing downstream can match
// on. [ERROR] is bold red because it is the one that must not be missed;
// [OK] green, [NOTE] blue, [WARN] and [HINT] yellow -- the two that mean
// "read this, nothing has stopped" share a colour because they are the
// same call on the reader's attention.
//
// Never into the log. The colouring is a property of the sink and not of
// the string, which is exactly why it is put on here, one line at a time,
// as the line goes to the terminal.
const (
	sgrReset  = "\x1b[0m"
	sgrBold   = "\x1b[1;31m"
	sgrRed    = "\x1b[31m"
	sgrGreen  = "\x1b[32m"
	sgrYellow = "\x1b[33m"
	sgrBlue   = "\x1b[34m"
)

// colours maps each marker to how it is shown.
var colours = map[string]string{
	MarkError: sgrBold,
	MarkOK:    sgrGreen,
	MarkNote:  sgrBlue,
	MarkWarn:  sgrYellow,
	MarkHint:  sgrYellow,
}

// Colour reports whether a stream may be coloured.
//
// Three questions, all of which have to answer yes. Is this really a
// terminal -- a redirected stream gets the bytes, and a file full of
// escape codes is a file every later reader has to know about. Has the
// reader asked for no colour: NO_COLOR is the convention that costs
// nothing to honour and is honoured by its presence, whatever it holds.
// And does the terminal claim to be able to show any: TERM=dumb says it
// cannot.
//
// The width is never asked. It can change during a run, and text folded
// to a number camp picked cannot be reflowed by whatever reads it later,
// so wrap folds to a fixed 68 columns and that is deliberate.
func Colour(stream io.Writer) bool {
	file, ok := stream.(*os.File)
	if !ok {
		return false
	}
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	_, err := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS)
	return err == nil
}

// paint colours the marker at the head of a line, and nothing else.
//
// A line that does not begin with a marker -- a refusal's continuation, a
// paragraph a command wrote itself -- is left exactly as it is.
func (s *Sink) paint(line string) string {
	if !s.colour {
		return line
	}
	end := strings.IndexByte(line, ']')
	if !strings.HasPrefix(line, "[") || end < 0 {
		return line
	}
	colour, known := colours[line[:end+1]]
	if !known {
		return line
	}
	return colour + line[:end+1] + sgrReset + line[end+1:]
}
