package privileged

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/mountx"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/verify"
)

// MountArg and UnmountArg are the internal subcommands sudo wraps. They
// are not commands anyone should type; the front end is the interface.
const (
	MountArg   = "helper-mount"
	UnmountArg = "helper-unmount"
)

// Helper reads a job from stdin, executes it, and writes the reply to
// stdout.
//
// This is the whole privileged surface of camp. It reads no
// configuration, runs no generator, consults no state and takes no
// arguments -- everything it acts on arrived on a pipe from a process
// that had already validated it, and it re-checks every one of those
// operands itself before touching anything.
func Helper(action Action, in io.Reader, out io.Writer) int {
	reply := Reply{Version: JobVersion}
	refuse := func(rule, format string, args ...any) int {
		reply.Rule = rule
		reply.Error = fmt.Sprintf(format, args...)
		return finish(out, reply, 1)
	}

	data, err := io.ReadAll(in)
	if err != nil {
		return refuse("helper-job-unreadable", "reading the job: %v", err)
	}

	// Strictly, and with nothing after it. This process is root, and a
	// field it does not understand is a field somebody expected it to
	// honour -- there is no version of that worth guessing at.
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var job Job
	if err := decoder.Decode(&job); err != nil {
		return refuse("helper-job-invalid", "the job does not parse: %v", err)
	}
	if decoder.More() {
		return refuse("helper-job-invalid",
			"the job is followed by more data, and this helper executes one job.")
	}

	if job.Version != JobVersion {
		return refuse("helper-job-version",
			"the job is version %d and this helper speaks version %d; the front "+
				"end and the helper are different builds of camp",
			job.Version, JobVersion)
	}
	if job.Action != action {
		return refuse("helper-job-action",
			"this helper was started for %q and the job asks for %q", action, job.Action)
	}

	// Who this is being done for. Taken from sudo rather than from the
	// job: the job arrives from an unprivileged process, and a uid in it
	// is a request for root to hand something to somebody, which is not a
	// request this program answers.
	uid, gid, err := invoker()
	if err != nil {
		return refuse("helper-no-invoker", "%v", err)
	}
	job.UID, job.GID = uid, gid

	if err := job.confine(); err != nil {
		var named ruled
		if errors.As(err, &named) {
			return refuse(named.rule, "%s", named.message)
		}
		return refuse("helper-job-invalid", "%v", err)
	}

	switch action {
	case ActionMount:
		reply = mount(job)
	case ActionUnmount:
		reply = unmount(job)
	default:
		reply.Error = fmt.Sprintf("unknown action %q", action)
	}

	code := 0
	if reply.Error != "" || len(reply.Stranded) > 0 {
		code = 1
	}
	return finish(out, reply, code)
}

func finish(out io.Writer, reply Reply, code int) int {
	reply.Version = JobVersion
	encoded, err := json.Marshal(reply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "camp helper: encoding the reply: %v\n", err)
		return 1
	}
	out.Write(append(encoded, '\n'))
	return code
}

// mount executes the plan, verifies it where it was built, and moves it.
//
// All three in one invocation, because sudo is exercised exactly once per
// command: giving the privilege back between mounting and moving and
// asking for it again would open a window in which a half-built tree
// exists and nothing is watching it.
func mount(job Job) Reply {
	reply := Reply{Version: JobVersion}
	var made []string

	for _, operation := range job.Mounts {
		target, sourceFD, targetFD, err := resolve(job, operation)
		if err != nil {
			reply.Error = err.Error()
			reply.Stranded = rollback(made)
			reply.RolledBack = len(reply.Stranded) == 0
			return reply
		}

		mountable := operation.AsMount(target)
		// The reopen: once the bind is made, the descriptor that made it
		// names what is underneath it, so the read-only remount and the
		// propagation change need the target opened again -- beneath the
		// same root, following nothing, as it was opened the first time.
		parts := operation.TargetParts
		placed, err := mountx.MountByDescriptor(mountable, sourceFD, targetFD,
			func() (int, error) {
				return pathx.OpenBeneath(job.Base, parts, unix.O_PATH)
			})
		closeAll(sourceFD, targetFD)
		// Recorded the moment the mount exists, not when the whole
		// operation finishes: a bind that succeeded and a remount that did
		// not leaves a mount standing, and a rollback that skipped it would
		// report a clean machine that is not clean.
		if placed {
			made = append(made, target)
		}
		if err != nil {
			reply.Error = err.Error()
			reply.Stranded = rollback(made)
			reply.RolledBack = len(reply.Stranded) == 0
			return reply
		}

		result := Result{Target: target, Outcome: "mounted"}
		if identity, err := identityOf(target); err == nil {
			result.Device, result.Inode = identity.Device, identity.Inode
		}
		reply.Results = append(reply.Results, result)
	}

	// The first verification pass, here rather than in the front end,
	// because the staging tree is where it has to be checked and the
	// privilege must not be given back in between.
	staging := filepath.Join(append([]string{job.Base}, job.StagingParts...)...)
	if problems := verifyStaging(job, staging); problems != "" {
		reply.Error = problems
		reply.Stranded = rollback(made)
		reply.RolledBack = len(reply.Stranded) == 0
		return reply
	}

	live := filepath.Join(append([]string{job.Base}, job.LiveParts...)...)
	if err := mountx.Move(staging, live); err != nil {
		reply.Error = err.Error()
		reply.Stranded = rollback(made)
		reply.RolledBack = len(reply.Stranded) == 0
		return reply
	}
	reply.Moved = true
	return reply
}

// verifyStaging runs the path-based pass against the tree where it was
// built.
func verifyStaging(job Job, staging string) string {
	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		return err.Error()
	}

	built := plan.Plan{Live: staging}
	for _, operation := range job.Mounts {
		target := filepath.Join(append([]string{job.Base}, operation.TargetParts...)...)
		mountable := operation.AsMount(target)
		mountable.InLive = false
		built.Mounts = append(built.Mounts, mountable)
	}

	problems := verify.Run(verify.Input{
		Plan:      built,
		Prefix:    staging,
		Table:     table,
		LowerPath: job.LowerPath,
		Storage:   job.Storage,
		Exclude:   job.Exclude,
		UID:       job.UID,
		GID:       job.GID,
	})
	if problems.Empty() {
		return ""
	}
	return problems.Error()
}

// resolve opens both ends of one operation without following a symlink,
// and checks that what it opened is what the front end saw.
func resolve(job Job, operation JobMount) (string, int, int, error) {
	target := filepath.Join(append([]string{job.Base}, operation.TargetParts...)...)

	targetFD, err := pathx.OpenBeneath(job.Base, operation.TargetParts, unix.O_PATH)
	if err != nil {
		return "", -1, -1, fmt.Errorf("opening the mount point %s: %w", target, err)
	}
	if err := checkIdentity(targetFD, target, operation.TargetIdent); err != nil {
		unix.Close(targetFD)
		return "", -1, -1, err
	}

	if len(operation.SourceParts) == 0 {
		return target, -1, targetFD, nil
	}
	sourceFD, err := pathx.OpenBeneath(job.Base, operation.SourceParts, unix.O_PATH)
	if err != nil {
		unix.Close(targetFD)
		return "", -1, -1, fmt.Errorf("opening the mount source %s: %w", operation.Source, err)
	}
	if err := checkIdentity(sourceFD, operation.Source, operation.SourceIdent); err != nil {
		closeAll(sourceFD, targetFD)
		return "", -1, -1, err
	}
	return target, sourceFD, targetFD, nil
}

// checkIdentity refuses when the object opened is not the object checked.
//
// This is the whole point of the helper re-resolving. Between the front
// end's validation and this moment a component the user owns could have
// been replaced -- with a symlink, or with an entirely different
// directory of the same name. The front end's checks would then have been
// about something else, and the mount would be about this.
func checkIdentity(fd int, path, expected string) error {
	if expected == "" {
		return nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("looking at %s: %w", path, err)
	}
	got := fmt.Sprintf("%d:%d", st.Dev, st.Ino)
	if got != expected {
		return fmt.Errorf("%s is not the object camp checked: it was %s and it "+
			"is now %s.\nSomething replaced it between the check and the mount. "+
			"Nothing has been mounted", path, expected, got)
	}
	return nil
}

type identity struct {
	Device uint64
	Inode  uint64
}

func identityOf(path string) (identity, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return identity{}, err
	}
	return identity{Device: uint64(st.Dev), Inode: st.Ino}, nil
}

func closeAll(descriptors ...int) {
	for _, fd := range descriptors {
		if fd >= 0 {
			unix.Close(fd)
		}
	}
}

// rollback removes what was mounted, in reverse, and returns what it
// could not remove.
func rollback(made []string) []string {
	var stranded []string
	for index := len(made) - 1; index >= 0; index-- {
		outcome, _ := mountx.Unmount(made[index])
		if outcome == mountx.Busy {
			stranded = append(stranded, made[index])
		}
	}
	return stranded
}

// unmount removes the recorded targets, in the order given.
//
// It never detaches anything lazily. A mount it cannot remove stays
// mounted, is reported as still mounted, and makes the command fail.
func unmount(job Job) Reply {
	reply := Reply{Version: JobVersion}
	for _, target := range job.Targets {
		outcome, err := mountx.Unmount(target)
		result := Result{Target: target, Outcome: string(outcome)}
		if err != nil && outcome == mountx.Busy {
			result.Error = err.Error()
			reply.Stranded = append(reply.Stranded, target)
		}
		reply.Results = append(reply.Results, result)
	}

	// The one thing the kernel creates as root: the overlay's leftover
	// work directory, mode 000. It is removed here, while there is still
	// the privilege to do it, so that the user is never left with a
	// directory of camp's that they cannot clear.
	if len(job.WorkParts) > 0 && len(reply.Stranded) == 0 {
		work := filepath.Join(append([]string{job.Base}, job.WorkParts...)...)
		// One directory, and only if camp made it. This is the single place
		// the helper removes anything or changes what it belongs to, and the
		// marker is what says the directory is camp's rather than whatever
		// the job's components happened to spell.
		if err := campsOwn(work); err != nil {
			reply.Error = err.Error()
			reply.Rule = err.(ruled).rule
			return reply
		}
		if err := fsx.Work(work).RemoveTree("work"); err != nil {
			reply.Error = err.Error()
		}
		if err := fsx.Work(work).Chown(job.UID, job.GID); err != nil && !os.IsNotExist(err) {
			reply.Error = err.Error()
		}
	}
	return reply
}
