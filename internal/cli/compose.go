package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
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
	"github.com/dlaszlo/camp/internal/prepare"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/report"
)

// composeCommands are the ones that mount something, and the ones that
// report on what is mounted.
func composeCommands() []command {
	return []command{
		{"run", "run a command inside the composition, without root", cmdRun},
		{"shell", "open a shell inside the composition, without root", cmdShell},
		{"status", "what is mounted, and what is not", cmdStatus},
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

// getReady performs the frame's first steps: take the locks, run the
// environment's own prepare commands, validate and gate while nothing is
// mounted, generate, and check what was generated.
//
// The order matters. The locks come first so that two camps racing can
// only refuse each other, and so that the prepare commands run inside
// what those locks protect. Validation comes after them, because a
// prepare command may have changed a repository and everything the
// validation reads has to be read as it will be mounted -- and it is
// still the one moment when repairing a repository by hand is safe:
// nothing is mounted, and nothing anybody does can land in the wrong
// place.
func getReady(cfg config.Config, say *report.Narrator) (*ready, error) {
	upper, upperOK := repositoryParts(cfg, cfg.Upper)
	if !upperOK {
		// The configuration does not name a usable upper; let validation
		// say so properly rather than failing on a lock.
		return nil, validationError(cfg)
	}

	// The one directory camp makes for the reader rather than asking them
	// to: the composed tree's own. It is named by the configuration, it is
	// always inside the environment root, and git cannot record an empty
	// directory -- so a clone of an environment could never bring it, and
	// every fresh checkout met a refusal for the one thing camp can safely
	// create itself. A path whose parent does not exist is still refused,
	// by the validation: that is a typo, not a missing directory.
	// Under the work lock until the live lock is held: between the two,
	// the composed tree's directory may not exist yet or may exist
	// unlocked, and a sweep looking at that moment would take this
	// launcher's work directory for one a finished session left.
	guard, err := lockWork(cfg)
	if err != nil {
		return nil, err
	}
	if err := makeLive(cfg, say); err != nil {
		guard.Release()
		return nil, err
	}

	pair, err := locks.TakePair(cfg.Env, upper, cfg.Merged.Components(),
		cfg.UpperPath(), cfg.Live())
	guard.Release()
	if err != nil {
		var single refusal.R
		if errors.As(err, &single) && strings.HasSuffix(single.Rule, "-locked") {
			return nil, failure(ExitBusy, "", "%s", single.Message)
		}
		// Anything else -- a missing directory, a symlink where a directory
		// was expected -- has a better message in the validation.
		return nil, validationError(cfg)
	}
	say.Locks(cfg.UpperPath(), cfg.Live())

	// The environment's own commands, here and nowhere else. After the
	// locks, so that what they check or fetch cannot be raced by a second
	// composition; before the plan, because one of them may change a
	// repository and the gate, the inventory and the generation step all
	// have to read the repositories as they will be mounted.
	if len(cfg.Prepare) > 0 {
		if problems := prepare.Run(cfg); !problems.Empty() {
			pair.Release()
			return nil, refusedComposition(problems)
		}
		if problems := stillTheComposition(cfg, pair); !problems.Empty() {
			pair.Release()
			return nil, refusedComposition(problems)
		}
		say.Prepared(len(cfg.Prepare))
	}

	built, refused := plan.Prepare(cfg)
	refused.Extend(runtimeChecks(built))
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

// lockWork takes the lock on camp's work area, .camp/work, making the
// directory first when it is not there.
//
// The third lock, and the shortest held: never by a session, only by a
// camp that is sweeping or starting. The sweep holds it while it decides
// what is stale and removes it; a launcher holds it from making the
// composed tree's directory to taking the live lock. Once the live lock
// is held the sweep's evidence stands on its own, so this goes before the
// prepare commands, which may take as long as they like.
func lockWork(cfg config.Config) (*locks.Held, error) {
	if _, err := fsx.In("work", cfg.Root, config.Dir, "work").MkdirAll(); err != nil {
		return nil, wrap(err, ExitFailure, "")
	}
	held, err := locks.Take(locks.Work, cfg.Env, []string{config.Dir, "work"},
		filepath.Join(cfg.Env, config.Dir, "work"))
	if err != nil {
		var single refusal.R
		if errors.As(err, &single) && single.Rule == "work-locked" {
			return nil, failure(ExitBusy, "", "%s", single.Message)
		}
		return nil, wrap(err, ExitFailure, "")
	}
	return held, nil
}

// stillTheComposition checks the two things the prepare commands were
// able to change and must not have.
//
// They are the environment's own programs, running as the invoking user,
// in the window between camp reading the configuration and locking two
// directories and camp deriving a plan from them. Nothing about them is
// hostile by assumption; the point is that the window exists at all, and
// that two of the things in it are the ones camp's guarantees rest on.
//
// A command that renames the code repository and puts another directory
// at the same path leaves camp holding a lock on an inode nothing will
// mount, while the composition it goes on to build is one no lock
// protects -- which is the state the locks exist to make impossible. The
// init re-checks the same thing against the locks it inherits, and this
// check is what lets the launcher refuse first, with a message about the
// prepare commands rather than about a file that changed.
//
// A command that edits the configuration leaves the launcher planning
// from one file and the process that mounts reading another. Compared by
// bytes, and the whole file: a change camp cannot see the meaning of is
// still a change it must not plan through.
func stillTheComposition(cfg config.Config, pair *locks.Pair) refusal.List {
	var refused refusal.List

	current, err := os.ReadFile(cfg.Source)
	switch {
	case err != nil:
		refused.Add("prepare-config-unreadable",
			"%s could not be read after the prepare commands ran: %v.\n"+
				"Nothing has been mounted. camp read that file, took the locks "+
				"from it and ran the commands it declares, and it has to know the "+
				"file is still the one it planned from. Look at what the commands "+
				"do to it, and start again.", cfg.Source, err)
		return refused
	case !bytes.Equal(cfg.Declared, current):
		refused.Add("prepare-config-changed",
			"%s changed while the prepare commands were running, and nothing "+
				"has been mounted.\ncamp read it, took the locks from it and ran "+
				"the commands the file declares; a plan derived now would not be "+
				"a plan of the file camp was asked about, and the process that "+
				"mounts reads the file again for itself. Look at what changed, "+
				"and start again -- the second run reads the file as it now is.",
			cfg.Source)
		return refused
	}

	for _, side := range []struct {
		held *locks.Held
		what string
		path string
	}{
		{pair.Upper, "code repository", cfg.UpperPath()},
		{pair.Live, "composed tree's directory", cfg.Live()},
	} {
		locked, err := side.held.Identity()
		if err != nil {
			refused.Add("prepare-lock-unreadable",
				"the lock camp holds on the %s could not be looked at after the "+
					"prepare commands ran: %v.\nNothing has been mounted.",
				side.what, err)
			continue
		}
		now, err := pathx.StatBeneath(side.path, nil)
		if err != nil {
			refused.Add("prepare-directory-unreadable",
				"the %s %s could not be looked at after the prepare commands "+
					"ran: %v.\nNothing has been mounted. camp locked that directory "+
					"before the commands ran and has to know it is still the same "+
					"one. Look at what the commands do to it, and start again.",
				side.what, side.path, err)
			continue
		}
		if now.Ident != locked {
			refused.Add("prepare-directory-replaced",
				"the %s at %s is a different directory from the one camp locked "+
					"before the prepare commands ran (%s against %s).\nNothing has "+
					"been mounted. A prepare command replaced it -- a rename and a "+
					"new directory at the same path is the usual way -- and camp's "+
					"lock is on the directory that is no longer there, so a "+
					"composition built now would be one nothing protects from a "+
					"second one. Look at what the commands do to %s, and start "+
					"again.",
				side.what, side.path, now.Ident, locked, side.path)
		}
	}
	return refused
}

func repositoryParts(cfg config.Config, name string) ([]string, bool) {
	repo, ok := cfg.Repository(name)
	if !ok {
		return nil, false
	}
	return repo.Path.Components(), true
}

func validationError(cfg config.Config) error {
	_, refused := plan.Prepare(cfg)
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
func runtimeChecks(built plan.Plan) refusal.List {
	var refused refusal.List
	if built.Live == "" {
		return refused
	}

	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		refused.Add("mount-table-unreadable", "%v", err)
		return refused
	}
	refused.Extend(locks.Residue(table, built.Live, built.Config.LowerPath(),
		built.Config.UpperPath()))
	return refused
}

func requireMachine() error {
	failed := preflight.Failures(preflight.Run())
	if len(failed) == 0 {
		return nil
	}
	details := make([]string, 0, len(failed))
	for _, check := range failed {
		details = append(details, check.Name+": "+check.Detail)
	}
	code := ExitPrecondition
	if failed[0].Name == "user namespaces" {
		code = ExitPrivilege
	}
	return failure(code, failed[0].Hint,
		"this machine cannot run camp -- %s", strings.Join(details, "; "))
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
