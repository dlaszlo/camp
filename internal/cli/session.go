package cli

import (
	"errors"
	"os"
	"strings"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/locks"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/preflight"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/report"
	"github.com/dlaszlo/camp/internal/session"
)

// The namespace mode: the primary way camp runs, and the one that needs
// no privilege.
func cmdRun(ctx *context, args []string) error {
	set, file := flagsFor("run")
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}
	return enter(ctx, *file, set.Args())
}

func cmdShell(ctx *context, args []string) error {
	set, file := flagsFor("shell")
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}
	return enter(ctx, *file, nil)
}

// enter builds the composition inside a namespace and hands over.
//
// Nothing is left behind when the session ends: the kernel discards the
// namespace and every mount in it. There is no down to run.
func enter(ctx *context, file string, argv []string) error {
	cfg, err := resolve(ctx, file)
	if err != nil {
		return err
	}
	if err := requireMachine(preflight.Namespace); err != nil {
		return err
	}

	// Before the sweep, and the order is the point. Judged from inside a
	// session, that session's own work directory looks stale -- its lock
	// is held outside this pid namespace and its live path is the
	// overlay's root, so nothing here can see it is held -- and the
	// refusal the locks give a moment later came after the removal.
	// Measured on a real environment: "swept: .../work/<hash>, left by a
	// session that has ended", then the upper lock refused with no holder
	// it could name, thirteen milliseconds apart.
	if err := notInside(cfg); err != nil {
		return err
	}

	// The sweep decides and removes under the work lock, and reads the
	// mount table under it too: compose.Sweep says what a table read
	// before the lock is worth.
	guard, err := lockWork(cfg)
	if err != nil {
		return err
	}
	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		guard.Release()
		var refused refusal.List
		refused.Add("mount-table-unreadable", "%v", err)
		return refusedComposition(refused)
	}
	sweep(report.Narrate(ctx.err), cfg, table)
	guard.Release()

	// The launcher's own steps, said as they finish. The rest of the
	// sequence -- the identity route, the mounts, their verification and
	// the environment -- is narrated by the init, which is where those
	// things happen and which is one sequential process, so the lines
	// arrive in the order the steps did.
	composition, err := getReady(cfg, plan.Namespace, report.Narrate(ctx.err))
	if err != nil {
		return err
	}
	defer composition.release()

	status, err := session.Launch(session.Options{
		Config: cfg,
		Argv:   argv,
		Locks:  composition.Locks,
		// The workload's own three streams, passed through untouched. They
		// are the program's, not camp's: what runs inside a session writes
		// to the terminal it was started from, and camp neither reads nor
		// keeps a copy of it.
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		var single refusal.R
		if errors.As(err, &single) {
			return failure(ExitPrecondition, "", "%s", single.Message)
		}
		return wrap(err, ExitFailure, "")
	}
	if status != 0 {
		os.Exit(status) // the workload's own status, not ours to reinterpret
	}
	return nil
}

// notInside refuses to enter a composition from inside it.
//
// The one check in this path that runs before anything acts, and it
// reads /proc rather than the locks, because from inside the session the
// locks are exactly what cannot be trusted.
func notInside(cfg config.Config) error {
	refused := session.FromInside(cfg.Source, cfg.Live())
	if refused.Empty() {
		return nil
	}
	// The same exit as a lock somebody else holds: the composition is
	// busy, and the one holding it is the session this was typed into.
	return failure(ExitBusy, "", "%s", refused.Error())
}

// sweep clears work directories left by sessions that are gone.
//
// The namespace mode has no down: the kernel tears the namespace down,
// mounts included, but camp's work directory is on the real filesystem
// and outlives it. So the next run sweeps. An entry whose marker cannot
// be read is reported and left alone -- camp removes only what it can
// prove is its own.
func sweep(say *report.Narrator, cfg config.Config, table []mountinfo.Entry) {
	swept, kept := compose.Sweep(cfg.Root, table, func(live string) bool {
		// Only absence proves the session is over. Every other error --
		// a permission, an I/O failure -- says camp could not find out, and
		// removing a work directory on the strength of not knowing would
		// take away the overlay's own work area from a session that is still
		// running.
		if _, err := os.Stat(live); err != nil {
			return os.IsNotExist(err)
		}
		held, err := locks.Take(locks.Live, "/", strings.Split(strings.TrimPrefix(live, "/"), "/"), live)
		if err != nil {
			return false // somebody is composing there
		}
		held.Release()
		return true
	})
	for _, directory := range swept {
		say.Swept(directory)
	}
	for _, note := range kept {
		say.LeftAlone(note)
	}
}
