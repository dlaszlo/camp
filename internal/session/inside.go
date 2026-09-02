package session

import (
	"os"
	"strings"

	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/refusal"
)

// FromInside refuses to start a session of this configuration from inside
// one.
//
// The fact comes from /proc/1/cmdline. The launcher clones the init into
// a pid namespace of its own, so the init is process 1 there -- Inside
// checks that of itself before it does anything else -- and the launcher
// hands it the configuration's absolute path as its first argument.
// Outside a session, process 1 is the machine's init. Both paths go
// through Real, so two spellings of one file agree.
//
// It is asked before anything else in a session's start because from
// inside, the two facts a start otherwise rests on are false: /proc/locks
// lists no lock whose owner is outside this pid namespace, and the
// composed tree's path is the overlay's root rather than the directory
// the launcher locked, so a flock on it succeeds. Measured: a 'camp shell'
// typed inside a session took the running session for one that had ended
// and swept its overlay's work directory away.
//
// When process 1 is a camp session init but the configuration it names
// cannot be resolved -- which is what a renamed environment directory
// looks like from inside, the recorded path no longer existing -- camp
// cannot tell whether this is the same composition, and it refuses. Not
// knowing is not permission: the alternative is going on to sweep on the
// chance that it is a different environment, and a sweep from inside the
// running session is the loss this whole check exists to stop.
//
// One case it does not catch: a session started from inside another
// (a different configuration, so this returns nothing) has no pre-refusal.
// It costs a message and not data -- the inner namespace inherits the
// outer's mount table, so the outer overlay's work directory is in it and
// the sweep's own mount-table guard keeps it.
func FromInside(source, live string) refusal.List {
	var refused refusal.List
	cmdline, err := os.ReadFile("/proc/1/cmdline")
	if err != nil {
		// Process 1's command line could not be read at all, so whether it
		// is camp's init is unknown. This is not the renamed-directory case
		// the refusal below is for; failing closed here would refuse every
		// ordinary run on a machine that hides it. Best effort, and proceed.
		return refused
	}
	given, state := initOf(cmdline, source)
	switch state {
	case sameComposition:
		refused.Add("inside-session",
			"a session is already running on %s, and this command was started "+
				"from inside it.\n"+
				"Process 1 of this pid namespace is camp's own session init, started "+
				"for %s -- the configuration this command was given -- and camp's "+
				"init is process 1 only inside the session it runs; outside, process "+
				"1 is the machine's init. So the shell this was typed into is in that "+
				"session, and the composed tree it shows is that session's.\n"+
				"A second session cannot be started from inside the first. From here "+
				"the first one's locks are invisible -- their holder is outside this "+
				"pid namespace -- and the composed tree's directory is the overlay's "+
				"root rather than the directory those locks are on, so camp would "+
				"take the running session for one that has ended. Nothing has been "+
				"mounted or removed.\n"+
				"To work in the running session, you already are. To start another, "+
				"leave this one first: exit the shell it opened, or use a terminal "+
				"that was not opened inside it, and run 'camp shell' or 'camp run' "+
				"there.",
			live, given)
	case unresolvedComposition:
		refused.Add("inside-session-unresolved",
			"camp will not start a session on %s from here: process 1 of this "+
				"pid namespace is camp's own session init, and camp cannot tell "+
				"whether this command was started from inside it.\n"+
				"That init names the configuration %s, which does not resolve to a "+
				"file camp can compare with this one. That is what a renamed "+
				"environment directory looks like from inside a running session: "+
				"the path the session recorded no longer exists.\n"+
				"camp will not go on to sweep on that footing. From inside a running "+
				"session its locks are invisible and its work directory looks "+
				"abandoned, so a sweep here could take the work area from a session "+
				"that is still running. Nothing has been mounted or removed.\n"+
				"If you are inside a session, leave it first: exit the shell it "+
				"opened, or use a terminal that was not opened inside it. If the "+
				"environment's directory was renamed while a session was running, "+
				"put the name back.",
			live, given)
	}
	return refused
}

// insideState is what process 1's command line says about whether this
// command is being run from inside a session of the given configuration.
type insideState int

const (
	// notInside: process 1 is not camp's init, or it is one running a
	// different configuration that camp could resolve and compare.
	notInside insideState = iota
	// sameComposition: process 1 is camp's init running this very
	// configuration.
	sameComposition
	// unresolvedComposition: process 1 is camp's init, but the
	// configuration one side names cannot be resolved, so camp cannot rule
	// out that it is this one.
	unresolvedComposition
)

// initOf reads what process 1's command line says, and returns the
// configuration path it names.
// initOf reports whether a process's command line is camp's session init
// running this configuration, and returns the configuration path it was
// given.
func initOf(cmdline []byte, source string) (string, insideState) {
	args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	if len(args) < 3 || args[1] != InitArg {
		return "", notInside
	}
	given := args[2]
	// Both sides resolved, and a failure on either is a refusal rather than
	// a pass: the init naming a path camp cannot resolve is the renamed
	// directory case, and letting a sweep run on it is the loss this
	// guards against.
	mine, err := pathx.Real(source)
	if err != nil {
		return given, unresolvedComposition
	}
	resolved, err := pathx.Real(given)
	if err != nil {
		return given, unresolvedComposition
	}
	if resolved != mine {
		return given, notInside
	}
	return given, sameComposition
}
