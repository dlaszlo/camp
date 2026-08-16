package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/holders"
	"github.com/dlaszlo/camp/internal/inventory"
	"github.com/dlaszlo/camp/internal/locks"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/preflight"
	"github.com/dlaszlo/camp/internal/privileged"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/report"
	"github.com/dlaszlo/camp/internal/session"
	"github.com/dlaszlo/camp/internal/state"
	"github.com/dlaszlo/camp/internal/verify"
)

// composeCommands are the ones that mount something, and the ones that
// report on what is mounted.
func composeCommands() []command {
	return []command{
		{"run", "run a command inside the composition, without root", cmdRun},
		{"shell", "open a shell inside the composition, without root", cmdShell},
		{"up", "assemble the composed tree for the whole machine", cmdUp},
		{"down", "take the composed tree apart", cmdDown},
		{"status", "what is mounted, and what is not", cmdStatus},
		{"list", "every recorded composition", cmdList},
		{"forget", "drop a composition's record; deletes nothing else", cmdForget},
		{"accept", "record the two repositories' root entries as they are now", cmdAccept},
	}
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

// ready is a composition that has passed everything that can be checked
// while nothing is mounted, with its locks held.
type ready struct {
	Config    config.Config
	Plan      plan.Plan
	Generated gen.Output
	Locks     *locks.Pair
}

// release drops the locks. It is safe to call twice: after a session
// hands them to its init, this process's copies are already gone.
func (r *ready) release() { r.Locks.Release() }

// prepare performs the frame's first four steps: take the locks,
// validate and gate while nothing is mounted, generate, and check what
// was generated.
//
// The order matters. The locks come first so that two camps racing can
// only refuse each other. The validation comes second, in the one moment
// when repairing a repository by hand is safe -- nothing is mounted, and
// nothing anybody does can land in the wrong place.
func prepare(cfg config.Config, mode plan.Mode) (*ready, error) {
	upper, upperOK := repositoryParts(cfg, cfg.Upper)
	if !upperOK {
		// The configuration does not name a usable upper; let validation
		// say so properly rather than failing on a lock.
		return nil, validationError(cfg, mode)
	}

	pair, err := locks.TakePair(cfg.Env, upper, cfg.Merged.Components(),
		cfg.UpperPath(), cfg.Live())
	if err != nil {
		var single refusal.R
		if errors.As(err, &single) && strings.HasSuffix(single.Rule, "-locked") {
			return nil, failure(ExitBusy, "", "%s", single.Message)
		}
		// Anything else -- a missing directory, a symlink where a directory
		// was expected -- has a better message in the validation.
		return nil, validationError(cfg, mode)
	}

	built, refused := plan.Prepare(cfg, mode)
	refused.Extend(runtimeChecks(built, mode))
	if !refused.Empty() {
		pair.Release()
		return nil, refusedComposition(refused)
	}

	if err := compose.Directories(built); err != nil {
		pair.Release()
		return nil, wrap(err, ExitFailure, "")
	}

	generated, problems := gen.Prepare(built)
	if !problems.Empty() {
		pair.Release()
		return nil, refusedComposition(problems)
	}

	return &ready{
		Config:    cfg,
		Plan:      gen.Expand(built, generated),
		Generated: generated,
		Locks:     pair,
	}, nil
}

func repositoryParts(cfg config.Config, name string) ([]string, bool) {
	repo, ok := cfg.Repository(name)
	if !ok {
		return nil, false
	}
	return repo.Path.Components(), true
}

func validationError(cfg config.Config, mode plan.Mode) error {
	_, refused := plan.Prepare(cfg, mode)
	if refused.Empty() {
		return failure(ExitFailure, "",
			"this composition could not be locked, and validation found nothing "+
				"wrong with it. That should not happen; run 'camp plan'.")
	}
	return refusedComposition(refused)
}

func refusedComposition(refused refusal.List) error {
	return failure(ExitPrecondition,
		"nothing was mounted, and nothing has to be undone -- every one of "+
			"these can be repaired by hand right now",
		"this composition cannot be brought up. %d thing(s) stop it:\n\n%s",
		len(refused), strings.TrimRight(report.Refusals(refused), "\n"))
}

// runtimeChecks are the refusals that need the machine's current state
// rather than the configuration's.
func runtimeChecks(built plan.Plan, mode plan.Mode) refusal.List {
	var refused refusal.List
	if built.Live == "" {
		return refused
	}

	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		refused.Add("mount-table-unreadable", "%v", err)
		return refused
	}
	refused.Extend(locks.Residue(table, built.Live, built.Config.LowerPath()))
	if mode == plan.Privileged {
		refused.Extend(locks.ScanUpper(table, built.Config.UpperPath()))
	}

	// A record in an active phase means a composition on this live path is
	// mounted, or was and did not finish coming down. Either way status
	// and down come before another up.
	record, found, err := state.Load(built.Hash)
	switch {
	case err != nil:
		refused.Add("record-unreadable",
			"the record for this composition could not be read: %v.\n"+
				"It is at %s. It names everything a teardown would have to remove, "+
				"so camp will not start a second composition without knowing what "+
				"the first one left.", err, state.Path(built.Hash))
	case found && record.Phase.Active():
		refused.Add("record-active",
			"there is already a record for this composed tree, in phase %q.\n"+
				"Run 'camp status' to see what is mounted, and 'camp down' to "+
				"remove it. The record is the only authoritative list of what a "+
				"teardown has to undo, and starting a second composition over it "+
				"would lose that list.", record.Phase)
	}
	return refused
}

// -- the namespace mode -----------------------------------------------------

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
	cfg, err := resolve(file)
	if err != nil {
		return err
	}
	if err := requireMachine(preflight.Namespace); err != nil {
		return err
	}

	sweep(ctx, cfg)

	composition, err := prepare(cfg, plan.Namespace)
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
func sweep(ctx *context, cfg config.Config) {
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
		ctx.printf("swept the leftover work directory %s\n", directory)
	}
	for _, note := range kept {
		fmt.Fprintf(ctx.err, "left alone: %s\n", note)
	}
}

// -- the privileged mode ----------------------------------------------------

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

func cmdStatus(ctx *context, args []string) error {
	set, file := flagsFor("status")
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}
	cfg, err := resolve(*file)
	if err != nil {
		return err
	}

	built, refused := plan.Prepare(cfg, plan.Namespace)
	table, tableErr := mountinfo.Read(mountinfo.Self)
	if tableErr != nil {
		return wrap(tableErr, ExitFailure, "")
	}

	if built.Live == "" {
		return refusedComposition(refused)
	}

	record, found, _ := state.Load(built.Hash)
	if found {
		ctx.printf("record: %s, phase %s, %s\n", state.Path(built.Hash),
			record.Phase, record.Age())
	} else {
		ctx.printf("no record: either nothing is up, or this is a namespace " +
			"session, which leaves none -- the namespace is the state, and it " +
			"goes with its last process.\n")
	}

	present := mountinfo.Under(table, built.Live)
	if len(present) == 0 {
		ctx.printf("down: nothing is mounted under %s.\n", built.Live)
		return nil
	}

	ctx.printf("\n%d mount(s) under %s:\n", len(present), built.Live)
	for _, entry := range present {
		ctx.printf("  %-8s %s\n", entry.FSType, entry.Point)
	}

	problems := verify.Run(verify.Input{
		Plan:   built,
		Prefix: built.Live,
		Table:  table,
		UID:    os.Getuid(),
		GID:    os.Getgid(),
	})
	if problems.Empty() {
		ctx.printf("\nup: every planned mount is present, reachable and the right " +
			"way round.\n")
		return nil
	}
	ctx.printf("\npartly up. %d thing(s) do not match the plan:\n\n%s",
		len(problems), report.Refusals(problems))
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
	var still []string
	for _, mount := range record.Mounts {
		if len(mountinfo.At(table, mount.Target)) > 0 {
			still = append(still, mount.Target)
		}
	}
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

// recordFor finds the record a teardown should act on.
func recordFor(file string) (state.Record, error) {
	cfg, err := resolve(file)
	if err != nil {
		return state.Record{}, err
	}
	live, hashErr := livePath(cfg)
	if hashErr != nil {
		return state.Record{}, hashErr
	}

	record, found, err := state.Load(plan.Hash(live))
	if err != nil {
		return state.Record{}, wrap(err, ExitFailure, "")
	}
	if !found {
		return state.Record{}, failure(ExitNotFound, "",
			"there is no record for %s, so there is nothing camp knows how to "+
				"take down.\nA namespace session leaves no record on purpose: it "+
				"ends when its last process exits, and the kernel removes every "+
				"mount with it. If something is mounted at %s all the same, 'camp "+
				"status' will show it.", live, live)
	}
	return record, nil
}

func livePath(cfg config.Config) (string, error) {
	live := cfg.Live()
	if _, err := os.Stat(live); err != nil {
		return "", failure(ExitNotFound, "", "%s does not exist", live)
	}
	return live, nil
}

func requireMachine(mode preflight.Mode) error {
	failed := preflight.Failures(preflight.Run(mode))
	if len(failed) == 0 {
		return nil
	}
	details := make([]string, 0, len(failed))
	for _, check := range failed {
		details = append(details, check.Name+": "+check.Detail)
	}
	code := ExitPrecondition
	if failed[0].Name == "privilege" || failed[0].Name == "user namespaces" {
		code = ExitPrivilege
	}
	return failure(code, failed[0].Hint,
		"this machine cannot run camp in %s mode -- %s", mode, strings.Join(details, "; "))
}
