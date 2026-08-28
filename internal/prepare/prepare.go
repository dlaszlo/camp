// Package prepare runs the environment's own programs, before anything of
// the composition exists.
//
// What is here and what is not. The `prepare:` list is code camp did not
// write, and camp takes exactly one thing from it: whether it succeeded.
// It produces no artefact, nothing reads its output, and nothing it does
// steers a mount -- which is what tells it apart from the generation
// step, whose output is treated as hostile data because a mount is made
// from it.
//
// It runs after the locks and before the plan. After the locks, so that
// whatever it checks or fetches happens while camp already holds the
// upper and the composed tree, and no second composition can start in
// that window. Before the plan, because a command that changes a
// repository has to be seen by the gate, the inventory and the
// generation step -- all of which read the repositories, and all of which
// must read them as they will be mounted.
//
// camp still writes nothing here. What runs is the environment's own
// program, as the invoking user, and it can write wherever that user can
// -- a repository included. That is the point of it, and it is the one
// place camp starts something with that reach, so the refusals say so.
package prepare

import (
	"fmt"
	"os"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/runx"
)

// Run runs every prepare command in order and stops at the first one
// that does not succeed.
//
// Stopping is deliberate, and it is the opposite of how the
// configuration's own validation behaves. That one reports every problem
// it can find, because it is a static read of a file with no side
// effects and one round of fixing beats four. These are programs that
// change things: running the next one after one has said stop is exactly
// the guessing camp does not do.
func Run(cfg config.Config) refusal.List {
	var refused refusal.List
	if len(cfg.Prepare) == 0 {
		return refused
	}

	// Never with privilege, for the reason the generation step gives:
	// whoever can edit the configuration would otherwise gain root through
	// it. In the privileged mode this cannot happen anyway -- these run in
	// the unprivileged front end -- and it is checked rather than assumed.
	if os.Geteuid() == 0 {
		refused.Add("prepare-privileged",
			"the prepare commands %s declares would run as root, and camp never "+
				"runs configured code with privilege.\n"+
				"Whoever can edit the configuration would otherwise gain root "+
				"through it. Run the same command again without sudo: camp's front "+
				"end runs them as you, and in the privileged mode only the mounting "+
				"itself is elevated.", cfg.Source)
		return refused
	}

	for _, command := range cfg.Prepare {
		result := runx.Run(runx.Command{
			Argv: command.Command,
			// The environment root. Not camp's scratch, which is where the
			// generation step is put so that a naive producer's relative
			// writes land somewhere harmless: a prepare command is asked to
			// check and to fetch rather than to make files, it is invoked as
			// a path of the environment, and every path it cares about is
			// under this directory. This gives up the generator's
			// safe-relative-write property, deliberately -- the scratch is
			// computable here (its name comes from the live path alone), and
			// it was not chosen.
			Dir: cfg.Env,
			// The invoking user's environment, plus the two paths camp can
			// state. The session: declarations are deliberately not here:
			// they are the workload's, and they are resolved against the
			// composed tree -- a value like $CAMP_LIVE/.record names nothing
			// at this point. A command that needs variables for its own
			// children exports them itself.
			// Appended rather than merged, and that is enough: where a name
			// appears twice, the child sees the last one. Measured on this
			// toolchain -- os/exec deduplicates the vector and keeps the
			// later value -- which matters because camp started from inside
			// another session inherits a CAMP_LIVE that must not win.
			Env: append(os.Environ(),
				"CAMP_ENV="+cfg.Env,
				"CAMP_LIVE="+cfg.Live()),
			Timeout: command.Timeout,
			// The child's own two streams, and camp's process globals on
			// purpose. These are not camp's reporting -- that goes through
			// report, which never reaches for a terminal -- they are the
			// descriptors a child inherits, and the same ones the generation
			// step hands its own child. Passing camp's narrating sink here
			// instead would put a pipe where the terminal was, and a program
			// that asks whether it is talking to a terminal would start
			// answering differently because camp was the one that started it.
			Stdout: os.Stdout,
			Stderr: os.Stderr,
		})
		if result.Outcome == runx.OK {
			continue
		}
		refused.Add(rule(result.Outcome), "%s", describe(cfg, command, result))
		return refused
	}
	return refused
}

func rule(outcome runx.Outcome) string {
	switch outcome {
	case runx.NotStarted:
		return "prepare-run"
	case runx.TimedOut:
		return "prepare-timeout"
	case runx.Interrupted:
		return "prepare-interrupted"
	default:
		return "prepare-failed"
	}
}

// describe says what happened to which command, and what it leaves
// behind.
//
// The argv vector is printed quoted, element by element: joined by
// spaces it could not be told apart from a vector whose elements contain
// them, and somebody reading a refusal is often about to retype it.
//
// The last part is what a generation step's message does not have to
// say. A generator writes into camp's scratch, so a failed one leaves the
// machine as it was; a prepare command is the environment's own program
// and may have changed a repository before it stopped. Saying "nothing
// has been mounted" and leaving it there would read as "nothing
// happened", which is a thing camp does not know.
func describe(cfg config.Config, command config.PrepareCommand, result runx.Result) string {
	var what string
	switch result.Outcome {
	case runx.NotStarted:
		what = "could not be started: " + result.Err.Error()
	case runx.TimedOut:
		what = "did not finish within " + command.Timeout.String() +
			", and its process group was killed"
	case runx.Interrupted:
		what = "was interrupted (" + result.Signal.String() +
			"), and its process group was sent the same signal"
	default:
		what = "failed: " + result.Err.Error()
	}

	return fmt.Sprintf("prepare command %d %s.\n"+
		"  command:     %q\n"+
		"  declared in: %s, line %d\n"+
		"  ran in:      %s\n"+
		"Nothing has been mounted, and the prepare commands after it did not "+
		"run. What it changed is still changed, and anything it left running "+
		"is still running: these are the environment's own programs, and camp "+
		"neither undoes nor knows what they did.\n"+
		"The command's own output above says what went wrong with it; camp "+
		"can only report that it did. Repair that, or the line in %s that "+
		"declares it, and run the same camp command again.",
		command.Index+1, what, command.Command,
		cfg.Source, command.Line, cfg.Env, cfg.Source)
}
