package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/inventory"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/report"
	"github.com/dlaszlo/camp/internal/state"
	"github.com/dlaszlo/camp/internal/verify"
)

// The commands that only look, and the one that records what they see.
func cmdExplain(ctx *context, args []string) error {
	set, file := flagsFor("explain")
	systemWide := set.Bool("privileged", false, "describe the system-wide mode")
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}
	cfg, err := resolve(*file)
	if err != nil {
		return err
	}
	// Every refusal, not only the ones that left no plan behind. explain
	// describes a tree to whoever is standing in it, and a description
	// rendered beside a standing refusal describes a tree that will not
	// exist -- which is worse than no description, because it reads as
	// authority.
	built, refused := plan.Prepare(cfg, parseMode(*systemWide))
	if !refused.Empty() || built.Live == "" {
		return refusedComposition(refused)
	}
	generated, _ := gen.Preview(built)
	ctx.printf("%s", report.Explain(gen.Expand(built, generated)))
	return nil
}

// cmdAccept takes the snapshot every up is compared against.
//
// Only this command writes it. An up that refreshed the file on the way
// past would swallow the very signal the file exists to raise: a new name
// at the workspace root changes what the derived read-only binds protect
// and what the exclude covers, and that has to be a change somebody
// looked at.
func cmdAccept(ctx *context, args []string) error {
	set, file := flagsFor("accept")
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}
	cfg, err := resolve(*file)
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

	if previous, found, err := inventory.Load(cfg.CampDir()); err == nil && found {
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

	if err := current.Save(cfg.CampDir()); err != nil {
		return wrap(err, ExitFailure, "")
	}
	ctx.printf("wrote %s: %d root entries across both repositories.\n"+
		"Its diff is meant to be read -- it is byte-sorted, one record per "+
		"line -- and camp compares against it at every up.\n",
		inventory.Path(cfg.CampDir()), len(current.Entries))
	return nil
}

// cmdStatus says what is on the machine, and it is the command a person
// reaches for when something has gone wrong. So it asks the record first
// and the configuration only when there is no record: the configuration
// may have been edited, or deleted, while the composition was up (§12).
func cmdStatus(ctx *context, args []string) error {
	set, file := flagsFor("status")
	live, hash := recoveryFlags(set)
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}

	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		return wrap(err, ExitFailure, "")
	}

	record, found, err := selectRecord(*file, *live, *hash)
	if err != nil {
		return err
	}
	for _, listing := range corruptRecords() {
		ctx.printf("corrupt: %s\n         %v\n\n", listing.Path, listing.Corrupt)
	}
	if found {
		return describeRecord(ctx, record, table)
	}
	return statusFromConfiguration(ctx, *file, table)
}

// statusFromConfiguration answers when no record claims this directory.
//
// The namespace mode leaves none by design -- the namespace is the state,
// and it goes with its last process -- and a composition that was never
// brought up leaves none either. Here the configuration is the only
// source there is, and it says one thing worth having: which tree to look
// at.
func statusFromConfiguration(ctx *context, file string, table []mountinfo.Entry) error {
	cfg, err := resolve(file)
	if err != nil {
		return err
	}

	built, refused := plan.Prepare(cfg, plan.Namespace)
	if built.Live == "" {
		return refusedComposition(refused)
	}

	ctx.printf("no record for %s. A namespace session leaves none: the "+
		"namespace is the state, and it goes with its last process. A "+
		"privileged composition always leaves one.\n", built.Live)

	present := mountinfo.Under(table, built.Live)
	if len(present) == 0 {
		ctx.printf("down: nothing is mounted under %s.\n", built.Live)
		return nil
	}

	ctx.printf("\n%d mount(s) under %s:\n", len(present), built.Live)
	for _, entry := range present {
		ctx.printf("  %-8s %s\n", entry.FSType, entry.Point)
	}

	// Compared against the plan this configuration derives today, in the
	// namespace mode -- which is what a session standing here was built
	// from. It is a comparison against a file rather than against a
	// record, and it says so, because the file may have moved on.
	problems := verify.Run(verify.Input{
		Plan:      built,
		Prefix:    built.Live,
		LowerPath: built.Config.LowerPath(),
		Storage:   built.Storage,
		Table:     table,
		UID:       os.Getuid(),
		GID:       os.Getgid(),
	})
	if problems.Empty() {
		ctx.printf("\nup: every mount the configuration plans is present, " +
			"reachable and the right way round.\n")
		return nil
	}
	ctx.printf("\n%d thing(s) do not match the plan this configuration derives "+
		"today:\n\n%s", len(problems), report.Refusals(problems))
	return failure(ExitPrecondition, "", "run 'camp down' to take it apart")
}

func cmdList(ctx *context, args []string) error {
	set := flag.NewFlagSet("list", flag.ContinueOnError)
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}

	listings := state.All()
	if len(listings) == 0 {
		ctx.printf("no records. A namespace session leaves none by design.\n")
		return nil
	}
	for _, listing := range listings {
		if listing.Corrupt != nil {
			ctx.printf("corrupt  %s\n         %v\n", listing.Path, listing.Corrupt)
			continue
		}
		ctx.printf("%-8s %s  (%s, %s)\n", listing.Record.Phase, listing.Record.Live,
			listing.Record.Hash, listing.Record.Age())
	}
	return nil
}

func cmdForget(ctx *context, args []string) error {
	set := flag.NewFlagSet("forget", flag.ContinueOnError)
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}
	if set.NArg() != 1 {
		return failure(ExitUsage, "",
			"'camp forget' takes one composition identifier. 'camp list' prints them.")
	}

	hash := set.Arg(0)
	record, found, err := state.Load(hash)
	if err != nil {
		return wrap(err, ExitFailure, "")
	}
	if !found {
		return failure(ExitNotFound, "", "there is no record %q", hash)
	}

	// Forgetting an active composition would discard the only
	// authoritative list of what has to be unmounted, which is down's to
	// consume and not forget's to lose.
	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		return wrap(err, ExitFailure, "")
	}
	still := state.StillMounted(record, table)
	if len(still) > 0 {
		return failure(ExitPrecondition, "",
			"%s is in phase %q and %d of its mounts are still present: %s.\n"+
				"This record is the only list of what a teardown has to remove. "+
				"Run 'camp down' first; forgetting it now would leave those mounts "+
				"with nothing that knows about them.",
			hash, record.Phase, len(still), strings.Join(still, ", "))
	}

	if err := state.Forget(hash); err != nil {
		return wrap(err, ExitFailure, "")
	}
	ctx.printf("forgot the record for %s. Nothing else was deleted: not the "+
		"repositories, not the storage, not the composed tree.\n", record.Live)
	return nil
}
