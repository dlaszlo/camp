package cli

import (
	"errors"
	"os"
	"strings"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/locks"
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

	sweep(report.Narrate(ctx.err), cfg)

	// The launcher's own steps, said as they finish. The rest of the
	// sequence -- the identity route, the mounts, their verification and
	// the environment -- is narrated by the init, which is where those
	// things happen and which is one sequential process, so the lines
	// arrive in the order the steps did.
	composition, err := prepare(cfg, plan.Namespace, report.Narrate(ctx.err))
	if err != nil {
		return err
	}
	defer composition.release()

	status, err := session.Launch(session.Options{
		Config:  cfg,
		Plan:    composition.Plan,
		Exclude: composition.Generated.Exclude,
		Argv:    argv,
		Locks:   composition.Locks,
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
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

// sweep clears work directories left by sessions that are gone.
//
// The namespace mode has no down: the kernel tears the namespace down,
// mounts included, but camp's work directory is on the real filesystem
// and outlives it. So the next run sweeps. An entry whose marker cannot
// be read is reported and left alone -- camp removes only what it can
// prove is its own.
func sweep(say *report.Narrator, cfg config.Config) {
	swept, kept := compose.Sweep(cfg.CampDir(), func(live string) bool {
		if _, err := os.Stat(live); err != nil {
			return true // the composed tree is gone; so is its session
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
