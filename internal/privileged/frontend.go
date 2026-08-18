package privileged

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/report"
	"github.com/dlaszlo/camp/internal/state"
	"github.com/dlaszlo/camp/internal/verify"
)

// RefuseRoot stops 'sudo camp up' before it can do any harm.
//
// Under root the generation step would run as root, which the design
// forbids outright, and everything camp created would be root-owned --
// including the storage the design guarantees the user can write. The
// front end is meant to be unprivileged from its first instruction to its
// last, and sudo wraps the helper alone.
func RefuseRoot(command string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	return refusal.New("run-as-root",
		"'camp %s' is running as root, and it must not.\n"+
			"camp elevates for one narrow step and no more: the front end locks, "+
			"validates, generates and writes its record as you, and only the "+
			"mounting itself is wrapped in sudo. Run it as root and the "+
			"generation step would run as root too -- which would hand a shell "+
			"to whoever can edit the configuration -- and everything camp "+
			"creates, including the storage you have to be able to write, would "+
			"belong to root.\nRun it without sudo:\n  camp %s", command, command)
}

// UpInput is what the front end needs, all of it produced unprivileged.
type UpInput struct {
	Plan    plan.Plan
	Exclude []byte
	Tool    string
	// ConfigBytes and InventoryBytes are digested into the record, so that
	// down can report drift without trusting the current files.
	ConfigBytes    []byte
	InventoryBytes []byte
	// Sudo is the command that elevates. It is a field so a test can hand
	// in something else; nothing else ever changes it.
	Sudo []string
	// Stderr is where sudo's prompt and the helper's diagnostics go.
	Stderr *os.File
	// Say narrates the steps this half of the mode performs. A nil one is
	// silent, which is what the tests want.
	Say *report.Narrator
}

// recordsOutsideRepositories refuses to keep the state record inside one
// of the repositories being composed.
//
// The record's directory is the user's own state directory, and
// XDG_STATE_HOME may name anything -- including a directory inside the
// code repository, in which case camp would write a file into a
// repository at every up. Nothing about the path is wrong enough for the
// filesystem to notice: it is the one place camp writes that it did not
// choose, so it is checked rather than confined.
func recordsOutsideRepositories(built plan.Plan) *refusal.R {
	directory := state.Dir()
	// Where the records would really land, not where the path spells it. A
	// lexical compare misses a symlinked XDG_STATE_HOME: the link aliases
	// the state directory into a repository while every lexical check stays
	// green. state resolved that base once and holds it open, so this is
	// the same directory the write will use rather than a second resolution
	// of the same name. If it could not be opened at all the writes will
	// fail with their own message, and the lexical path is the most this
	// check can honestly say.
	resolved := directory
	if real, err := state.Location(); err == nil {
		resolved = real
	}
	for _, repository := range built.Config.Repositories {
		root := repository.Path.Join(built.Config.Env)
		if !pathx.Under(resolved, root) {
			continue
		}
		problem := refusal.New("state-in-repository",
			"camp's records would be written to %s, which is inside the "+
				"repository %q (%s).\n"+
				"That directory is where the privileged mode keeps what it has to "+
				"undo, and camp writes into no repository, ever. It is chosen by "+
				"XDG_STATE_HOME, or by $HOME/.local/state when that is unset. Point "+
				"XDG_STATE_HOME somewhere outside the repositories and run this "+
				"again.", directory, repository.Name, root)
		return &problem
	}
	return nil
}

// Left says what is on the machine now. Only Up knows: the same failure
// list is reached from exits that removed everything again and from exits
// that deliberately left the composition standing, and the sentence that
// closes a failed run has to be true about the machine the reader is
// sitting at.
type Left int

const (
	// Clean: nothing of this composition is mounted, and the workspace is
	// not held read-only.
	Clean Left = iota
	// Standing: mounts are on the machine and the workspace is read-only
	// for it. This is what success looks like, and also what the failures
	// after the move look like -- they leave the tree in place rather than
	// half-removing it.
	Standing
	// Uncertain: camp could not find out. The helper died without a reply,
	// so it may have stopped before its first mount or after its last, and
	// guessing either way would be a sentence about a machine nobody
	// looked at.
	Uncertain
)

// Up builds the composition for the whole machine.
func Up(in UpInput) (Left, refusal.List) {
	var refused refusal.List
	built := in.Plan

	if problem := recordsOutsideRepositories(built); problem != nil {
		refused.Push(*problem)
		return Clean, refused
	}
	if err := compose.Directories(built); err != nil {
		refused.Add("directories", "%v", err)
		return Clean, refused
	}

	work := fsx.Work(built.Config.Root, built.Hash)
	staging, err := work.MkdirAllMode(0o700, "staging")
	if err != nil {
		refused.Add("staging", "%v", err)
		return Clean, refused
	}

	// The record goes down before anything is mounted, carrying the whole
	// plan. From here on, whatever happens, something knows what to undo.
	record := state.FromPlan(built, in.Tool,
		state.Digest(in.ConfigBytes), state.Digest(in.InventoryBytes),
		os.Getuid(), os.Getgid())
	record.Staging = staging
	if err := record.Save(); err != nil {
		refused.Add("record", "%v", err)
		return Clean, refused
	}
	in.Say.Record(state.Path(built.Hash))

	job, problems := MountJob(built, staging, in.Exclude)
	if !problems.Empty() {
		_ = state.Forget(built.Hash)
		return Clean, problems
	}

	in.Say.Helper()
	reply, err := run(in.Sudo, MountArg, job, in.Stderr)
	switch {
	case err != nil:
		refused.Add("helper", "%v", err)
		record.Phase = state.Partial
		_ = record.Save()
		return Uncertain, refused
	case reply.Error != "":
		if reply.RolledBack {
			_ = state.Forget(built.Hash)
			refused.Add("mount-failed",
				"%s\nNothing is mounted: the helper removed everything it had "+
					"made before it stopped.", reply.Error)
			return Clean, refused
		}
		record.Phase = state.Partial
		_ = record.Save()
		refused.Add("mount-failed-partial",
			"%s\nThe rollback could not finish. Still mounted: %s.\n"+
				"Run 'camp status' to see what is there, and 'camp down' to remove "+
				"it once whatever is holding it has let go.",
			reply.Error, strings.Join(reply.Stranded, ", "))
		return Standing, refused
	}

	in.Say.Mounted(len(reply.Results), staging)

	record.Mounts = merge(record.Mounts, reply.Results)
	if err := record.Save(); err != nil {
		refused.Add("record", "%v", err)
		return Standing, refused
	}

	// The second pass, and the one that decides: the move is the moment
	// the tree becomes machine-visible, and only a path-based check at the
	// final location can prove what an outside process now sees.
	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		refused.Add("mount-table-unreadable", "%v", err)
		return Standing, refused
	}
	problems = verify.Run(verify.Input{
		Plan:      built,
		Prefix:    built.Live,
		LowerPath: built.Config.LowerPath(),
		Storage:   built.Storage,
		Table:     table,
		Exclude:   in.Exclude,
		UID:       os.Getuid(),
		GID:       os.Getgid(),
	})
	if !problems.Empty() {
		record.Phase = state.Partial
		_ = record.Save()
		problems.Add("verify-after-move",
			"The composition is mounted at %s and did not pass the check that "+
				"runs after it becomes visible to the rest of the machine. It is "+
				"left in place rather than half-removed: run 'camp down'.", built.Live)
		return Standing, problems
	}

	in.Say.Moved(staging, built.Live)
	in.Say.Verified(len(built.Mounts), built.Live)

	record.Phase = state.Up
	if err := record.Save(); err != nil {
		refused.Add("record", "%v", err)
	}
	return Standing, refused
}

// Down takes a recorded composition apart.
//
// It reads the record and never the configuration. The configuration may
// have been edited while the composition was up, and then the file that
// says what to unmount would describe a composition nobody built.
func Down(record state.Record, sudo []string, stderr *os.File) (Reply, refusal.List) {
	var refused refusal.List
	reply, err := run(sudo, UnmountArg, UnmountJob(record), stderr)
	if err != nil {
		refused.Add("helper", "%v", err)
		return reply, refused
	}
	return reply, refused
}

// UnmountJob builds the teardown instruction from the record, and from
// nothing else.
//
// This is what makes recovery possible after a crash: the record carries
// the complete concrete plan, so down never needs the configuration --
// which may have been edited, or deleted, while the composition was up.
// The targets come out in the reverse of the order they were mounted in.
func UnmountJob(record state.Record) Job {
	targets := make([]JobTarget, 0, len(record.Mounts)+len(record.Detached))
	for _, mount := range record.Teardown() {
		targets = append(targets, JobTarget{
			Path:   mount.Target,
			Device: mount.Device,
			Inode:  mount.Inode,
		})
	}
	// Last, and after everything that stood on them: the mount points the
	// helper bound onto themselves so the composition could be moved into
	// place without propagating. One of them is the live path itself, which
	// the composition was covering until a moment ago. They carry no
	// identity: each was covered by the composition for its whole life, so
	// nothing could ever look at one.
	for _, path := range record.Detached {
		targets = append(targets, JobTarget{Path: path})
	}

	job := Job{
		Version: JobVersion,
		Action:  ActionUnmount,
		Base:    record.Env,
		UID:     record.UID,
		GID:     record.GID,
		Targets: targets,
	}
	work := filepath.Join(record.Env, config.Dir, "work", record.Hash)
	if parts, ok := relativeTo(record.Env, work); ok {
		job.WorkParts = parts
	}
	return job
}

// MountJob turns the plan into the instruction the helper executes,
// recording what each operand was when the front end looked at it.
//
// Exported beside UnmountJob so that what crosses into the privileged half
// can be inspected without elevating anything: it is a plain value, and a
// test asserting what is *not* in it is worth more than a comment saying
// the same.
func MountJob(built plan.Plan, staging string, exclude []byte) (Job, refusal.List) {
	var refused refusal.List

	stagingParts, ok := relativeTo(built.Config.Env, staging)
	if !ok {
		refused.Add("staging-outside",
			"the staging directory %s is not inside the environment root %s.",
			staging, built.Config.Env)
		return Job{}, refused
	}

	job := Job{
		Version:      JobVersion,
		Action:       ActionMount,
		Base:         built.Config.Env,
		UID:          os.Getuid(),
		GID:          os.Getgid(),
		StagingParts: stagingParts,
		LiveParts:    built.Config.Merged.Components(),
		LowerPath:    built.Config.LowerPath(),
		WorkParts:    workParts(built),
		Storage:      built.Storage,
		Exclude:      exclude,
	}

	for _, mount := range built.Mounts {
		targetParts := mount.TargetParts
		target := mount.Target
		if mount.InLive {
			targetParts = append(append([]string{}, stagingParts...), mount.Rel.Components()...)
			target = mount.Rel.Join(staging)
		}

		operation := JobMount{
			Kind:        string(mount.Kind),
			Role:        string(mount.Role),
			Source:      mount.Source,
			Target:      target,
			SourceParts: mount.SourceParts,
			TargetParts: targetParts,
			Lower:       mount.Lower,
			Upper:       mount.Upper,
			Work:        mount.Work,
			Xattr:       mount.Xattr,
			SourceType:  string(mount.Type),
		}
		if mount.Kind != plan.Overlay && len(mount.SourceParts) > 0 {
			identity, err := identityBeneath(built.Config.Env, mount.SourceParts)
			if err != nil {
				refused.Add("source-vanished",
					"the mount source %s could not be looked at: %v.", mount.Source, err)
				continue
			}
			operation.SourceIdent = identity
		}
		if mount.Kind == plan.Overlay {
			if problems := overlayOperands(&operation, built); !problems.Empty() {
				refused.Extend(problems)
				continue
			}
		}

		// A mount point that is not there yet is the ordinary case: most of
		// them are supplied by an earlier mount of this same sequence. What
		// must not happen is the two being confused, so absence is recorded
		// as absence and anything else stops the job here, while nothing is
		// mounted.
		switch identity, err := identityBeneath(built.Config.Env, targetParts); {
		case err == nil:
			operation.TargetIdent = identity
		case errors.Is(err, os.ErrNotExist):
			operation.TargetAbsent = true
		default:
			refused.Add("target-vanished",
				"the mount point %s could not be looked at: %v.", target, err)
			continue
		}
		job.Mounts = append(job.Mounts, operation)
	}
	return job, refused
}

// overlayOperands fills in the three directories the composed tree is
// made of, as components beneath the base and as identities.
//
// Every one of them has to exist by now: the two repositories are the
// composition's own, and the work directory was created a moment ago by
// compose.Directories. So a failure to look at one is a refusal rather
// than a case to carry.
func overlayOperands(operation *JobMount, built plan.Plan) refusal.List {
	var refused refusal.List
	take := func(path, what string) ([]string, string, bool) {
		parts, ok := relativeTo(built.Config.Env, path)
		if !ok {
			refused.Add("overlay-operand-outside",
				"the overlay's %s is %s, which is not inside the environment root "+
					"%s.\nEvery operand the helper mounts is addressed as components "+
					"beneath that root, so that root can never be pointed at "+
					"something outside the composition.", what, path, built.Config.Env)
			return nil, "", false
		}
		identity, err := identityBeneath(built.Config.Env, parts)
		if err != nil {
			refused.Add("overlay-operand-vanished",
				"the overlay's %s %s could not be looked at: %v.", what, path, err)
			return nil, "", false
		}
		return parts, identity, true
	}

	for _, lower := range operation.Lower {
		parts, identity, ok := take(lower, "lower layer")
		if !ok {
			return refused
		}
		operation.LowerParts = append(operation.LowerParts, parts)
		operation.LowerIdents = append(operation.LowerIdents, identity)
	}
	if operation.Upper == "" {
		return refused
	}
	parts, identity, ok := take(operation.Upper, "upper layer")
	if !ok {
		return refused
	}
	operation.UpperParts, operation.UpperIdent = parts, identity

	parts, identity, ok = take(operation.Work, "work directory")
	if !ok {
		return refused
	}
	operation.WorkParts, operation.WorkIdent = parts, identity
	return refused
}

// workParts names camp's work directory beneath the environment root, so
// that a failed mount can clear what the kernel leaves inside it. Only a
// privileged process can, and this is the only one there is.
func workParts(built plan.Plan) []string {
	parts, ok := relativeTo(built.Config.Env, built.Work)
	if !ok {
		return nil
	}
	return parts
}

func identityBeneath(base string, parts []string) (string, error) {
	fd, err := pathx.OpenBeneath(base, parts, unix.O_PATH)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), nil
}

func relativeTo(base, path string) ([]string, bool) {
	if !strings.HasPrefix(path, base+"/") {
		return nil, false
	}
	return strings.Split(strings.TrimPrefix(path, base+"/"), "/"), true
}

// run invokes the helper through sudo, handing it the job on stdin.
//
// Never on argv: /proc exposes a process's arguments to every user on the
// machine, and the job names every path of the composition. stdin is a
// pipe between two processes and nothing else can read it.
func run(sudo []string, action Action, job Job, stderr *os.File) (Reply, error) {
	encoded, err := json.Marshal(job)
	if err != nil {
		return Reply{}, fmt.Errorf("encoding the job: %w", err)
	}

	self, err := os.Executable()
	if err != nil {
		return Reply{}, fmt.Errorf("finding this binary: %w", err)
	}

	argv := append(append([]string{}, sudo...), self, string(action))
	command := exec.Command(argv[0], argv[1:]...)
	command.Stdin = bytes.NewReader(encoded)
	command.Stderr = stderr
	command.Env = append(os.Environ(), "LC_ALL=C")

	output, err := command.Output()
	if len(output) == 0 {
		if err != nil {
			return Reply{}, fmt.Errorf("the privileged helper did not run: %w\n"+
				"It is invoked as: %s", err, strings.Join(argv, " "))
		}
		return Reply{}, fmt.Errorf("the privileged helper said nothing")
	}

	var reply Reply
	if err := json.Unmarshal(bytes.TrimSpace(output), &reply); err != nil {
		return Reply{}, fmt.Errorf("the privileged helper's reply did not "+
			"parse: %w\n%s", err, output)
	}
	return reply, nil
}

// merge folds what the helper reported into the recorded plan, so that
// the record says not only what was planned but what exists.
//
// By position, not by path: the helper walks the job's mounts in order
// and the job mirrors the plan, while the paths deliberately differ --
// the helper worked in the staging tree and the record names where the
// mounts end up.
func merge(mounts []state.Mount, results []Result) []state.Mount {
	for index := range mounts {
		if index >= len(results) {
			break
		}
		mounts[index].Device = results[index].Device
		mounts[index].Inode = results[index].Inode
	}
	return mounts
}
