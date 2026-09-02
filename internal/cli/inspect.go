package cli

import (
	"fmt"
	"os"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/inventory"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/report"
)

// The commands that only look, and the one that records what they see.
//
// explain is generated from the live configuration, so that it cannot go
// stale (§16). A session leaves no record of its own -- the namespace is
// the state, and it goes with its last process -- so the configuration is
// the only source there is, and it is the one the session standing here
// was built from.
func cmdExplain(ctx *context, args []string) error {
	set, file := flagsFor("explain")
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}

	cfg, err := resolve(ctx, *file)
	if err != nil {
		return err
	}
	// Every refusal, not only the ones that left no plan behind. explain
	// describes a tree to whoever is standing in it, and a description
	// rendered beside a standing refusal describes a tree that will not
	// exist -- which is worse than no description, because it reads as
	// authority.
	built, refused := plan.Prepare(cfg)
	if !refused.Empty() || built.Live == "" {
		return refusedComposition(refused)
	}
	generated, _ := gen.Preview(built)
	ctx.printf("%s", report.Explain(gen.Expand(built, generated)))
	return nil
}

// cmdAccept takes the snapshot every start is compared against.
//
// Only this command writes it. A start that refreshed the file on the way
// past would swallow the very signal the file exists to raise: a new name
// at the workspace root changes what the derived read-only binds protect
// and what the exclude covers, and that has to be a change somebody
// looked at.
func cmdAccept(ctx *context, args []string) error {
	set, file := flagsFor("accept")
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}
	cfg, err := resolve(ctx, *file)
	if err != nil {
		return err
	}

	lower, err := pathx.ReadDirBeneath(cfg.LowerPath(), nil)
	if err != nil {
		return wrap(fmt.Errorf("listing the workspace %s: %w", cfg.LowerPath(), err),
			ExitPrecondition, "")
	}
	upper, err := pathx.ReadDirBeneath(cfg.UpperPath(), nil)
	if err != nil {
		return wrap(fmt.Errorf("listing the code repository %s: %w", cfg.UpperPath(), err),
			ExitPrecondition, "")
	}
	current := inventory.Take(lower, upper)

	if previous, found, err := inventory.Load(cfg.Root); err == nil && found {
		differences := inventory.Compare(previous, current)
		if len(differences) == 0 {
			ctx.printf("nothing has changed since the last snapshot.\n")
			return nil
		}
		ctx.printf("accepting %d change(s):\n", len(differences))
		for _, difference := range differences {
			ctx.printf("  %s\n", difference.Describe())
		}
	}

	if err := current.Save(cfg.Root); err != nil {
		return wrap(err, ExitFailure, "")
	}
	ctx.printf("wrote %s: %d root entries across both repositories.\n"+
		"Its diff is meant to be read -- it is byte-sorted, one record per "+
		"line -- and camp compares against it at every start.\n",
		inventory.Path(cfg.Env), len(current.Entries))
	return nil
}

// cmdStatus says what is mounted at the composed tree's path, from where
// the command is run.
//
// A session's mounts exist only inside its namespace (C20), so from
// outside every session this reports nothing mounted, and that is the true
// answer for the process asking. From inside a session it answers for the
// session: the configuration says which tree to look at, and the
// verification pass says whether what stands there is what the
// configuration plans.
func cmdStatus(ctx *context, args []string) error {
	set, file := flagsFor("status")
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}

	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		return wrap(err, ExitFailure, "")
	}
	cfg, err := resolve(ctx, *file)
	if err != nil {
		return err
	}

	built, refused := plan.Prepare(cfg)
	if built.Live == "" {
		return refusedComposition(refused)
	}

	present := mountinfo.Under(table, built.Live)
	if len(present) == 0 {
		ctx.printf("nothing is mounted under %s, as seen from this process.\n"+
			"A session's mounts exist only inside its namespace: from outside, "+
			"a running session shows nothing here, and a second 'camp shell' "+
			"on this tree is refused while one runs. From inside a session this "+
			"command describes it.\n", built.Live)
		return nil
	}

	ctx.printf("%d mount(s) under %s:\n", len(present), built.Live)
	for _, entry := range present {
		ctx.printf("  %-8s %s\n", entry.FSType, entry.Point)
	}

	// Compared against the plan this configuration derives today, which is
	// what a session standing here was built from. It is a comparison
	// against a file, and it says so, because the file may have moved on
	// since the session started.
	//
	// Through the same call a composition is built with, deliberately:
	// status is that pass with the other exit -- reporting instead of
	// refusing -- and a second assembly of the same input beside it is a
	// second definition of what "up" means, free to drift from the first.
	problems := compose.Check(compose.Setup{
		Plan: built,
		UID:  os.Getuid(),
		GID:  os.Getgid(),
	})
	if problems.Empty() {
		ctx.printf("\nup: every mount the configuration plans is present, " +
			"reachable and the right way round.\n")
		return nil
	}
	ctx.printf("\n%d thing(s) do not match the plan this configuration derives "+
		"today:\n\n%s", problems.Count(), report.Refusals(problems))
	return failure(ExitPrecondition,
		"a session's mounts go when its last process exits: leave the "+
			"session, and start it again from the configuration as it now is",
		"what is mounted here is not what %s plans", cfg.Source)
}
