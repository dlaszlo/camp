package cli

import (
	"os"
	"strings"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/drift"
	"github.com/dlaszlo/camp/internal/holders"
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

	cfg, err := resolve(*file)
	if err != nil {
		return err
	}
	if err := requireMachine(preflight.Privileged); err != nil {
		return err
	}

	up, err := prepare(cfg, plan.Privileged)
	if err != nil {
		return err
	}
	defer up.release()

	ctx.printf("privileged mode: %s is read-only for the whole machine until "+
		"'camp down'.\n"+
		"  One mount table means there is no inside and no outside here: either "+
		"the workspace is held read-only for every process, your editor "+
		"included, or a process in the tree could write it by absolute path. "+
		"The protection wins. Normal work runs in the namespace mode, where "+
		"both promises hold.\n\n", cfg.LowerPath())

	configBytes, _ := os.ReadFile(cfg.Source)
	refused := privileged.Up(privileged.UpInput{
		Plan:        up.Plan,
		Exclude:     up.Generated.Exclude,
		Tool:        Version,
		ConfigBytes: configBytes,
		Sudo:        []string{"sudo"},
		Stderr:      os.Stderr,
	})
	if !refused.Empty() {
		return failure(ExitFailure, "", "%s", strings.TrimRight(report.Refusals(refused), "\n"))
	}

	ctx.printf("%s is up: %d mounts, all verified at the live path.\n",
		up.Plan.Live, len(up.Plan.Mounts))
	return nil
}

func cmdDown(ctx *context, args []string) error {
	set, file := flagsFor("down")
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}
	if err := privileged.RefuseRoot("down"); err != nil {
		return failure(ExitUsage, "", "%s", err)
	}

	cfg, err := resolve(*file)
	if err != nil {
		return err
	}
	record, err := recordFor(*file)
	if err != nil {
		return err
	}

	reply, refused := privileged.Down(record, []string{"sudo"}, os.Stderr)
	if !refused.Empty() {
		return failure(ExitFailure, "", "%s", strings.TrimRight(report.Refusals(refused), "\n"))
	}

	removed := 0
	for _, result := range reply.Results {
		if result.Outcome == "unmounted" {
			removed++
		}
	}
	ctx.printf("%d of %d mounts removed.\n", removed, len(reply.Results))

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

	record.Phase = state.Down
	_ = record.Save()

	// The four read-only scans, while the cause is still fresh. They never
	// block: down may only report.
	if built, refused := plan.Prepare(cfg, plan.Privileged); refused.Empty() || built.Live != "" {
		if found := drift.Refresh(built); !found.Empty() {
			ctx.printf("\n%s", found.String())
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
