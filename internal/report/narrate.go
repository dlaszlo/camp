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

// Narrate returns a narrator writing to a stream. A nil writer -- or a nil
// narrator -- says nothing, so callers never have to ask whether to speak.
func Narrate(out io.Writer) *Narrator { return &Narrator{out: out} }

func (n *Narrator) say(format string, args ...any) {
	if n == nil || n.out == nil {
		return
	}
	fmt.Fprintf(n.out, "%s\n", wrap(fmt.Sprintf(format, args...), "  "))
}

// Locks: taken first, so that two camps racing can only refuse each other.
func (n *Narrator) Locks(upper, live string) {
	n.say("locks: held on %s and on %s, for as long as this composition lasts",
		upper, live)
}

// Checked: everything a composition can be refused for while nothing is
// mounted -- the moment when a repair by hand is still safe.
func (n *Narrator) Checked(mounts int) {
	n.say("checked: %d mounts derived and validated, the two repository roots "+
		"gated against each other, nothing refused", mounts)
}

// Generated: the artefacts, produced as the invoking user and before
// anything is mounted.
func (n *Narrator) Generated(has bool) {
	if !has {
		n.say("generation: this composition declares no generation step, so it " +
			"has no exclude")
		return
	}
	n.say("generated: the exclude and the islands lists, as you, before any " +
		"mount exists")
}

// Identity: which uid route the namespace takes, and the one fact about
// this run that surprises somebody later.
func (n *Narrator) Identity(session config.Session) {
	n.say("identity: %s -- %s", nsx.For(session.Identity).Describe(), OwnershipClause)
}

// Environment: the names applied, never their values. What a variable
// holds is between the configuration, the terminal it was started from,
// and the workload.
func (n *Narrator) Environment(names []string) {
	if len(names) == 0 {
		return
	}
	n.say("environment: %s -- applied to the workload after the mount "+
		"capability was given back, and to nothing else", strings.Join(names, ", "))
}

// Mounted: the sequence, and the verification that decides.
func (n *Narrator) Mounted(count int, where string) {
	n.say("mounted: %d, in order, at %s", count, where)
}

// Verified: what the checks found by asking the kernel through paths,
// rather than by trusting the calls that were made.
func (n *Narrator) Verified(where string) {
	n.say("verified: every mount present, reachable and the right way round "+
		"at %s", where)
}

// Record: the privileged mode's teardown list, written before anything is
// mounted so that whatever happens next, something knows what to undo.
func (n *Narrator) Record(path string) {
	n.say("record: %s carries the whole plan, so a teardown never needs the "+
		"configuration", path)
}

// Helper: the one elevated step, and the only one.
func (n *Narrator) Helper() {
	n.say("helper: the mounts run through one sudo'd 'camp helper-mount' -- " +
		"the front end you are talking to stays unprivileged from start to " +
		"finish")
}

// Moved: the moment the tree becomes visible to the machine.
func (n *Narrator) Moved(live string) {
	n.say("moved: the staged tree onto %s, and checked again there -- only a "+
		"check at the final path can prove what an outside process now sees", live)
}

// MachineWide: the two effects this mode has outside the composition,
// stated as facts now in force.
func (n *Narrator) MachineWide(workspace, live string) {
	n.say("machine-wide: %s is read-only for every process on this machine, "+
		"your editor included, until 'camp down'", workspace)
	n.say("machine-wide: %s is visible to every process on this machine", live)
}

// Announcement: the session section this mode does not apply.
func (n *Narrator) Announcement(session config.Session) {
	if !session.Present {
		return
	}
	n.say("%s", Announcement())
}
