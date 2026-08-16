package report

import (
	"fmt"
	"io"
	"strings"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/nsx"
)

// Narrator says what a command is doing, in order, as each frame step
// completes.
//
// Why the commands narrate at all: the two modes differ in what they
// start, what they make visible, and to whom -- and no amount of
// configuration structure makes that legible at the moment somebody is
// using one. A short line per step does, and it lands in the terminal
// scrollback and in captured logs where a question asked later is
// actually answered.
//
// The rules these lines are held to: they say what happened, never what
// might; they name no hypothetical; and they print no inherited
// environment value, only the names of the variables that were applied.
// They go to stderr, so that plan output and report text on stdout stay
// pipeable.
type Narrator struct{ out io.Writer }

// Every line begins with what it is. Without that, a run reads as one
// undifferentiated block in which the step that failed looks exactly like
// the seven that did not -- and a line saying nothing is mounted, which is
// the aftermath of a failure, reads as a success. The marker is the first
// thing on the line because that is the column an eye runs down.
const (
	MarkOK    = "[OK]"
	MarkNote  = "[NOTE]"
	MarkError = "[ERROR]"
	MarkHint  = "[HINT]"
	// markWidth aligns the text, so the prose starts in one column
	// whichever marker precedes it.
	markWidth = len(MarkError) + 1
)

// Marked renders one marked line, aligned.
//
// Everything starts in one of two columns: the marker in the first, the
// text in the second, and every line a message runs onto in the second as
// well. Nothing is indented relative to anything else -- a block whose
// lines start in three different places is harder to read than a long
// one, and the eye running down the marker column is the whole point of
// having markers.
//
// Exported so a command's ending reads in the same columns as the steps
// that led to it: a failure rendered in some other shape is a failure
// somebody has to hunt for.
func Marked(marker, text string) string {
	indent := strings.Repeat(" ", markWidth)
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimSpace(line)
	}
	return fmt.Sprintf("%-*s%s", markWidth, marker,
		wrap(strings.Join(lines, "\n"), indent))
}

// Narrate returns a narrator writing to a stream. A nil writer -- or a nil
// narrator -- says nothing, so callers never have to ask whether to speak.
func Narrate(out io.Writer) *Narrator { return &Narrator{out: out} }

// say is one completed step.
func (n *Narrator) say(format string, args ...any) {
	n.mark(MarkOK, fmt.Sprintf(format, args...))
}

// Note is something true that is not a step: what a mode does not do,
// what a section does not apply to.
func (n *Narrator) Note(format string, args ...any) {
	n.mark(MarkNote, fmt.Sprintf(format, args...))
}

// Failed is what stopped the command, said as plainly as the steps that
// led to it.
func (n *Narrator) Failed(format string, args ...any) {
	n.mark(MarkError, fmt.Sprintf(format, args...))
}

func (n *Narrator) mark(marker, text string) {
	if n == nil || n.out == nil {
		return
	}
	fmt.Fprintf(n.out, "%s\n", Marked(marker, text))
}

// Locks: taken first, so that two camps racing can only refuse each other.
func (n *Narrator) Locks(upper, live string) {
	n.say("locks: %s, %s", upper, live)
}

// Checked: everything a composition can be refused for while nothing is
// mounted -- the moment when a repair by hand is still safe.
func (n *Narrator) Checked(mounts int) {
	n.say("checked: %d mounts, gate clean, nothing refused", mounts)
}

// Generated: the artefacts, produced as the invoking user and before
// anything is mounted.
func (n *Narrator) Generated(has bool) {
	if !has {
		n.say("generation: none declared, so this composition has no exclude")
		return
	}
	n.say("generated: the exclude and the islands lists")
}

// Identity: which uid route the namespace takes, and the one fact about
// this run that surprises somebody later.
func (n *Narrator) Identity(session config.Session) {
	n.say("identity: %s; files owned by anyone else show as nobody",
		nsx.For(session.Identity).Short())
}

// Environment: the names applied, never their values. What a variable
// holds is between the configuration, the terminal it was started from,
// and the workload.
func (n *Narrator) Environment(names []string) {
	if len(names) == 0 {
		return
	}
	n.say("environment: %s", strings.Join(names, ", "))
}

// Mounted: the sequence, and the verification that decides.
func (n *Narrator) Mounted(count int, where string) {
	n.say("mounted: %d at %s", count, where)
}

// Verified: what the checks found by asking the kernel through paths,
// rather than by trusting the calls that were made.
func (n *Narrator) Verified(count int, where string) {
	n.say("verified: %d mounts at %s", count, where)
}

// Record: the privileged mode's teardown list, written before anything is
// mounted so that whatever happens next, something knows what to undo.
func (n *Narrator) Record(path string) {
	n.say("record: %s", path)
}

// Helper: the one elevated step, and the only one.
func (n *Narrator) Helper() {
	n.say("helper: sudo camp helper-mount")
}

// Moved: the moment the tree becomes visible to the machine.
func (n *Narrator) Moved(staging, live string) {
	n.say("moved: %s -> %s", staging, live)
}

// MachineWide: the two effects this mode has outside the composition,
// stated as facts now in force.
func (n *Narrator) MachineWide(workspace, live string) {
	n.Note("machine-wide: %s is read-only until 'camp down'", workspace)
	n.Note("machine-wide: %s is visible to every process", live)
}

// Announcement: the session section this mode does not apply.
func (n *Narrator) Announcement(session config.Session) {
	if !session.Present {
		return
	}
	n.Note("session: not applied here; 'camp run' and 'camp shell' apply it")
}
