package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/drift"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/inventory"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/report"
	"github.com/dlaszlo/camp/internal/session"
)

// The commands that only look, and the one that records what they see.
//
// explain is generated from the live configuration, so that it cannot go
// stale (§16). A session leaves no record of its own -- the namespace is
// the state, and it goes when the session ends -- so the configuration is
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
	//
	// The one refusal that is not a reason to withhold the description is
	// live-not-empty when this command is run from inside the very session
	// it describes: there the live path is the overlay's own root and shows
	// the composed tree, so the start-time emptiness precondition -- which
	// exists so an overlay does not hide user content -- fires against
	// exactly the state explain is for. Any other refusal still stops it,
	// and from outside the refusal stands as before.
	built, refused := plan.Prepare(cfg)
	if built.Live == "" {
		return refusedComposition(refused)
	}
	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		return wrap(err, ExitFailure, "")
	}
	if blocking := standingRefusals(refused, insideSession(cfg, built.Live, table)); !blocking.Empty() {
		return refusedComposition(refused)
	}
	generated, _ := gen.Preview(built)
	ctx.printf("%s", report.Explain(gen.Expand(built, generated), session.Grace))
	return nil
}

// insideSession reports whether this process stands inside a running
// session of this configuration at this live path.
//
// Two facts, and both are needed. Process 1 being camp's init for this
// configuration says the process is in a session started from this file;
// an overlay standing at the live path in this process's own mount table
// says the tree the file now names is the one that is mounted. The first
// alone rests on the file's name: edit merged: in the same file and the
// old session would vouch for a path it never composed, letting a command
// suppress a real refusal and describe the new path as authority.
func insideSession(cfg config.Config, live string, table []mountinfo.Entry) bool {
	if session.FromInside(cfg.Source, live).Empty() {
		return false
	}
	for _, entry := range mountinfo.AllOverlays(table) {
		if entry.Point == live {
			return true
		}
	}
	return false
}

// standingRefusals is what still stops a read-only command: every refusal
// the plan derived, minus live-not-empty when the command stands inside the
// session whose overlay makes the live directory non-empty. Exactly that
// one, and only there.
func standingRefusals(refused refusal.List, inside bool) refusal.List {
	var blocking refusal.List
	for _, r := range refused {
		if inside && r.Rule == "live-not-empty" {
			continue
		}
		blocking.Push(r)
	}
	return blocking
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
// session: the configuration says which tree to look at, the verification
// pass says whether what stands there is what the configuration plans, and
// the drift pass says what has changed under it since it started.
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
	// The one refusal a running session makes of its own live directory is
	// tolerated from inside it; every other is reported below, because a
	// configuration that would now be refused -- a declaration that no
	// longer resolves, an edited prepare: command -- is not "up", whatever
	// is mounted.
	blocking := standingRefusals(refused, insideSession(cfg, built.Live, table))

	// Expanded to the full sequence a session really mounts: the islands a
	// generation step contributes are not in the bare plan, so a comparison
	// against the bare plan would report every island mount as one the plan
	// does not have. gen.Preview derives them without mounting anything,
	// exactly as plan and explain do -- and its exclude is the payload a
	// session mounts, byte for byte (the init checks its output against the
	// same assembly), so the artefact check runs here too.
	generated, _ := gen.Preview(built)
	built = gen.Expand(built, generated)

	present := mountinfo.Under(table, built.Live)
	if len(present) == 0 {
		ctx.printf("nothing is mounted under %s, as seen from this process.\n"+
			"A session's mounts exist only inside its namespace: from outside, "+
			"a running session shows nothing here, and a second 'camp shell' "+
			"on this tree is refused while one runs. From inside a session this "+
			"command describes it.\n", built.Live)
		if !blocking.Empty() {
			return refusedComposition(refused)
		}
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
		Plan:    built,
		Exclude: generated.Exclude,
		UID:     os.Getuid(),
		GID:     os.Getgid(),
	})
	if problems.Empty() {
		ctx.printf("\nup: every mount the configuration plans is present, " +
			"reachable and the right way round.\n")
	} else {
		ctx.printf("\n%d thing(s) do not match the plan this configuration "+
			"derives today:\n\n%s", problems.Count(), report.Refusals(problems))
	}
	if !blocking.Empty() {
		ctx.printf("\nthe configuration as it now stands would be refused, so the "+
			"next start would not build this tree. %d thing(s) stop it:\n\n%s",
			blocking.Count(), report.Refusals(blocking))
	}

	// The drift pass, so a reader can ask at any time what has gone stale
	// under a running session, not only at its end. It is the same
	// read-only scan the farewell runs (drift.Refresh: the gate re-run, the
	// inventory comparison, the untracked and index scans), so the
	// mid-session answer and the end-of-session answer cannot disagree.
	//
	// Unbounded, unlike the farewell: a status command has no promised
	// length to keep, so a git that hung would hang the command a reader is
	// watching -- and can interrupt -- rather than the init that holds the
	// locks.
	found := drift.Refresh(built, time.Time{})
	if !found.Empty() {
		ctx.printf("\nwhat has changed under this running session:\n\n%s", found.String())
	}

	repairs := statusRepairs(problems, blocking, found)
	if len(repairs) == 0 {
		return nil
	}
	return failure(ExitPrecondition, strings.Join(repairs, " "),
		"what is inside this session is no longer all that %s now describes", cfg.Source)
}

// statusRepairs is the closing advice, one sentence per kind of thing
// found, each the true repair for its case. Nothing found needing a hand
// is an empty list and a clean exit.
//
// The kinds need different repairs, and one sentence for all of them was
// wrong for two: a worktree registered under the composed tree is repaired
// with the printed command, and ending the session is exactly what breaks
// its registration; a new workspace root entry is absorbed by no restart --
// the next start refuses it until 'camp accept' records it. Worktrees alone
// are reported and not counted against the exit: they are a standing
// arrangement with a command beside it, not the tree gone stale.
func statusRepairs(problems, blocking refusal.List, found drift.Report) []string {
	var repairs []string
	if !problems.Empty() {
		repairs = append(repairs, "A running session is built once and does not "+
			"follow the file: a changed root file, configuration or bin/ program is "+
			"absorbed by the next start. End the session (exit its shell or "+
			"command) and start it again.")
	}
	if !blocking.Empty() {
		repairs = append(repairs, "Repair the configuration before the next start; "+
			"what is mounted here is unaffected until the session ends.")
	}
	if found.Inventory != "" {
		repairs = append(repairs, "A changed root entry is not absorbed by a restart: "+
			"look at it, then record it with 'camp accept' -- the next start refuses "+
			"until you do.")
	}
	if len(found.Overlaps) > 0 || len(found.Untracked) > 0 || len(found.Indexed) > 0 ||
		len(found.Failures) > 0 {
		repairs = append(repairs, "Look at the paths named above before committing; "+
			"a leak caught here is usually still free to undo.")
	}
	if len(found.Worktrees) > 0 && len(repairs) == 0 {
		// Nothing else needs a hand, and the worktree's own repair command is
		// printed above it. Ending the session is not the advice here.
		return nil
	}
	return repairs
}
