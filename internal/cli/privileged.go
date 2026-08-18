package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/drift"
	"github.com/dlaszlo/camp/internal/holders"
	"github.com/dlaszlo/camp/internal/locks"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/preflight"
	"github.com/dlaszlo/camp/internal/privileged"
	"github.com/dlaszlo/camp/internal/report"
	"github.com/dlaszlo/camp/internal/state"
)

// The system-wide mode: an unprivileged front end and one narrow helper.
func cmdUp(ctx *context, args []string) error {
	set, file := flagsFor("up")
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}
	if err := privileged.RefuseRoot("up"); err != nil {
		return failure(ExitUsage, "", "%s", err)
	}

	cfg, err := resolve(ctx, *file)
	if err != nil {
		return err
	}
	if err := requireMachine(preflight.Privileged); err != nil {
		return err
	}

	// The price of this mode, before the first sudo prompt rather than
	// after it: nobody should meet a machine-wide read-only workspace as a
	// surprise.
	fmt.Fprintf(ctx.err, "privileged mode: %s is read-only for the whole "+
		"machine until "+
		"'camp down'.\n"+
		"  One mount table means there is no inside and no outside here: either "+
		"the workspace is held read-only for every process, your editor "+
		"included, or a process in the tree could write it by absolute path. "+
		"The protection wins. Normal work runs in the namespace mode, where "+
		"both promises hold.\n\n", cfg.LowerPath())

	// The steps say what they did as they finish, on stderr. The two modes
	// differ in what they start and what they make visible, and this is
	// where that difference is legible: at the moment of use, in the
	// scrollback, and in whatever captured the run.
	say := report.Narrate(ctx.err)

	up, err := prepare(cfg, plan.Privileged, say)
	if err != nil {
		return err
	}
	defer up.release()

	configBytes, _ := os.ReadFile(cfg.Source)
	left, refused := privileged.Up(privileged.UpInput{
		Plan:        up.Plan,
		Exclude:     up.Generated.Exclude,
		Tool:        Version,
		ConfigBytes: configBytes,
		Sudo:        []string{"sudo"},
		// The helper's own stream, and the terminal's rather than camp's
		// sink: sudo writes its password prompt here without a newline
		// after it, and a line-oriented sink would hold that prompt until
		// the line it never finishes.
		Stderr: os.Stderr,
		Say:    say,
	})
	if !refused.Empty() {
		// Said as plainly as the steps that led here. A run that ends with a
		// sentence about what is not mounted, in the same shape as seven
		// lines about what went right, reads as a success.
		//
		// And it says what is true of this failure rather than of failure in
		// general: the exits after the move leave the composition standing on
		// purpose, so a fixed sentence about a clean machine told the reader
		// the workspace was writable while it was held read-only, three lines
		// above the refusal saying it was still mounted.
		switch left {
		case privileged.Clean:
			say.Failed("camp up failed. Nothing of this composition is mounted, and "+
				"%s is writable again.", cfg.LowerPath())
		case privileged.Standing:
			say.Failed("camp up failed, and what it built is still on the machine: "+
				"%s stays read-only until it comes down.", cfg.LowerPath())
		default:
			say.Failed("camp up failed, and camp cannot say what it left behind: "+
				"%s may still be read-only. 'camp status' says what is there, and "+
				"'camp down' removes it.", cfg.LowerPath())
		}
		return failure(ExitFailure, "", "%s", strings.TrimRight(report.Refusals(refused), "\n"))
	}

	say.MachineWide(cfg.LowerPath(), up.Plan.Live)
	say.Announcement(cfg.Session)

	say.Done("camp up finished: %s is up, %d mounts, all verified at the live "+
		"path. 'camp down' takes it apart.", up.Plan.Live, len(up.Plan.Mounts))
	return nil
}

func cmdDown(ctx *context, args []string) error {
	set, file := flagsFor("down")
	live, hash := recoveryFlags(set)
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}
	if err := privileged.RefuseRoot("down"); err != nil {
		return failure(ExitUsage, "", "%s", err)
	}

	noteUnreadableConfiguration(ctx, *file)

	record, found, err := selectRecord(*file, *live, *hash)
	if err != nil {
		return err
	}
	// A teardown may run with no readable configuration at all -- that is
	// what the record is for -- so the log is opened from the record's own
	// environment rather than from a file that may be gone. It is the run
	// most worth having a record of: nothing else says what came down.
	if found {
		ctx.keepUnder(record.Config)
	}
	if !found {
		return failure(ExitNotFound, "",
			"no record for this directory, so there is nothing camp "+
				"knows how to take down.\nA namespace session leaves no record on "+
				"purpose: it ends when its last process exits, and the kernel "+
				"removes every mount with it. 'camp list' prints the compositions "+
				"that do have a record, and 'camp status' says what is mounted "+
				"here.")
	}

	// os.Stderr and not camp's sink, for the reason cmdUp gives: this is
	// where sudo asks for a password.
	reply, refused := privileged.Down(record, []string{"sudo"}, os.Stderr)
	if !refused.Empty() {
		return failure(ExitFailure, "", "%s", strings.TrimRight(report.Refusals(refused), "\n"))
	}

	removed := 0
	var mismatched []string
	for _, result := range reply.Results {
		switch result.Outcome {
		case "unmounted":
			removed++
		case "mismatch":
			mismatched = append(mismatched, result.Error)
		}
	}
	say := report.Narrate(ctx.err)
	for _, result := range reply.Results {
		if result.Outcome == "unmounted" {
			say.Unmounted(result.Target)
		}
	}
	say.Done("%d of %d mounts removed.", removed, len(reply.Results))

	if len(reply.Stranded) > 0 {
		record.Phase = state.Partial
		_ = record.Save()
		ctx.printf("\n")
		for _, target := range reply.Stranded {
			ctx.printf("%s", compose.DescribeStuck(compose.Stuck{
				Target:  target,
				Reason:  "the kernel refused to remove it",
				Holders: holders.Find(target),
			}))
		}
		return failure(ExitBusy, "",
			"the composition is partly up: %d mount(s) are still there.\n"+
				"camp does not detach a busy mount to make this look clean -- it "+
				"would leave the mount alive and still being written through while "+
				"disappearing from the kernel's table. Deal with the holder above "+
				"and run 'camp down' again.", len(reply.Stranded))
	}

	// A mount that is not camp's is left standing, and that is a failure of
	// the teardown rather than a detail of it: the record still names a
	// path camp cannot account for, so it stays.
	if len(mismatched) > 0 {
		record.Phase = state.Partial
		_ = record.Save()
		return failure(ExitFailure, "",
			"%s\nThe record is kept: it is the only account of what this "+
				"composition put where. Look at what is mounted there, and run "+
				"'camp down' again once it is gone -- or 'camp forget %s' if you "+
				"have decided camp's mount is not coming back.",
			strings.Join(mismatched, "\n\n"), record.Hash)
	}

	// The helper's own last step -- clearing the kernel's root-owned work
	// directory -- can fail after every unmount succeeded. Saying nothing
	// about it would be a success reported over a teardown that did not
	// finish.
	if reply.Error != "" {
		record.Phase = state.Partial
		_ = record.Save()
		return failure(ExitFailure, "",
			"every mount came down and the teardown did not finish: %s\n"+
				"The record is kept until it does.", reply.Error)
	}

	record.Phase = state.Down
	_ = record.Save()

	// The four read-only scans, while the cause is still fresh. They never
	// block: down may only report. Unlike the teardown above, they do need
	// the configuration -- they compare the repositories against what it
	// declares -- so when the file is gone or no longer describes this
	// composition they are skipped, and said to be skipped. A silent
	// omission would read as "no drift found".
	switch cfg, err := config.Load(record.Config); {
	case record.Config == "" || err != nil:
		ctx.printf("\nthe drift and leak scans need the configuration and were "+
			"skipped: %s cannot be read now. The teardown needed none of it.\n",
			record.Config)
	default:
		if built, _ := plan.Prepare(cfg, plan.Privileged); built.Live != record.Live {
			ctx.printf("\nthe drift and leak scans were skipped: %s no longer "+
				"describes the composition that was here.\n", record.Config)
		} else if found := drift.Refresh(built); !found.Empty() {
			ctx.printf("\n%s", found.String())
		}
	}

	// The work directory is disposable and this is what disposes of it:
	// the overlay's workdir, the generated exclude, the islands expansion
	// and the staging point, none of which outlive the composition. The
	// namespace mode has no down and sweeps at the next up instead.
	//
	// Under the live lock, and skipped when it cannot be taken. Nothing
	// holds it once everything is unmounted -- unless another camp is
	// starting a composition on this same live path at this moment, and
	// its work directory has this same name, because the name is derived
	// from the live path. Removing that one would take the overlay's work
	// area out from under a composition being built.
	if held, err := locks.Take(locks.Live, "/",
		strings.Split(strings.TrimPrefix(record.Live, "/"), "/"), record.Live); err != nil {
		say.LeftAlone(fmt.Sprintf("%s: something is composing at %s, so this "+
			"run did not remove it. The next 'camp up' here sweeps it.",
			plan.WorkDir(record.Env, record.Hash), record.Live))
	} else {
		// The record's environment, opened here: a teardown works from the
		// record and never from a configuration, so there is no cfg.Root to
		// borrow and this is the one place the path becomes a capability.
		err := removeWorkDir(record)
		held.Release()
		if err != nil {
			say.LeftAlone(fmt.Sprintf("%s could not be removed: %v",
				plan.WorkDir(record.Env, record.Hash), err))
		} else {
			say.Removed(plan.WorkDir(record.Env, record.Hash))
		}
	}

	if leftovers, err := compose.Residue(record.Live); err == nil && len(leftovers) > 0 {
		ctx.printf("\n%s is not empty after unmounting: %s\n"+
			"That is evidence, not rubbish: something wrote there while the "+
			"composition was up, or a mount did not cover what it should have. "+
			"camp leaves it exactly where it is.\n",
			record.Live, strings.Join(leftovers, ", "))
	}
	_ = state.Forget(record.Hash)
	return nil
}

// removeWorkDir clears one composition's work directory from the
// environment the record names.
//
// The path is resolved and opened once, here, and the removal is
// addressed from that descriptor: a teardown runs long after the
// composition went up, which is all the time anybody needs to rename the
// environment away and leave a link at its name.
func removeWorkDir(record state.Record) error {
	root, err := pathx.OpenRoot(record.Env)
	if err != nil {
		return err
	}
	defer root.Close()
	return compose.RemoveWorkDir(root, record.Hash)
}
