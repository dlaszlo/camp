package cli

import (
	"errors"
	"os"
	"strings"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/locks"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/preflight"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/report"
	"github.com/dlaszlo/camp/internal/state"
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
		{"explain", "describe the composed tree to whoever is standing in it", cmdExplain},
	}
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
func prepare(cfg config.Config, mode plan.Mode, say *report.Narrator) (*ready, error) {
	upper, upperOK := repositoryParts(cfg, cfg.Upper)
	if !upperOK {
		// The configuration does not name a usable upper; let validation
		// say so properly rather than failing on a lock.
		return nil, validationError(cfg, mode)
	}

	// The one directory camp makes for the reader rather than asking them
	// to: the composed tree's own. It is named by the configuration, it is
	// always inside the environment root, and git cannot record an empty
	// directory -- so a clone of an environment could never bring it, and
	// every fresh checkout met a refusal for the one thing camp can safely
	// create itself. A path whose parent does not exist is still refused,
	// by the validation: that is a typo, not a missing directory.
	if err := makeLive(cfg, say); err != nil {
		return nil, err
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
	say.Locks(cfg.UpperPath(), cfg.Live())

	built, refused := plan.Prepare(cfg, mode)
	refused.Extend(runtimeChecks(built, mode))
	if !refused.Empty() {
		pair.Release()
		return nil, refusedComposition(refused)
	}
	say.Checked(len(built.Mounts))
	// Said here, by the command that is actually composing the tree. These
	// were computed at every up and shown by nobody: only 'camp plan' and
	// 'camp doctor' printed them, and neither is what somebody runs before
	// starting work.
	say.Warnings(built.Warnings)

	if err := compose.Directories(built); err != nil {
		pair.Release()
		return nil, wrap(err, ExitFailure, "")
	}

	generated, problems := gen.Prepare(built)
	if !problems.Empty() {
		pair.Release()
		return nil, refusedComposition(problems)
	}
	_, generates := cfg.GenerationStep()
	say.Generated(generates)

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
		refused.Count(), strings.TrimRight(report.Refusals(refused), "\n"))
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

// makeLive creates the composed tree's directory when it is not there.
//
// Only the commands that compose call this. Planning executes nothing, so
// 'camp plan' reports the absence and creates nothing -- which is also
// why this is a warning in the validation rather than a refusal.
func makeLive(cfg config.Config, say *report.Narrator) error {
	live := cfg.Live()
	if _, err := os.Stat(live); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return wrap(err, ExitFailure, "")
	}
	// Checked here and not only in the validation, because this runs
	// before it: a merged: that points inside a repository would otherwise
	// have camp create a directory in one, which is the first invariant
	// and the thing fsx exists to make impossible. The validation refuses
	// it properly a moment later, with the message that explains it.
	for _, repo := range cfg.Repositories {
		if pathx.Under(live, repo.Path.Join(cfg.Env)) {
			return failure(ExitPrecondition, "",
				"the composed tree's directory %s is inside the repository %q, so "+
					"camp will not create it. Point merged: beside the repositories, "+
					"not into one.", live, repo.Name)
		}
	}
	if err := fsx.Live(cfg.Root, cfg.Merged.Components()...).Ensure(0o755); err != nil {
		return failure(ExitPrecondition, "",
			"the composed tree's directory %s could not be created: %v", live, err)
	}
	say.Created(live)
	return nil
}
