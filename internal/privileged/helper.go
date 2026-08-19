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

	// The base is opened here, once, and held for the rest of this
	// invocation. Nothing after this line names it again: every operand,
	// every reopen and the work directory are resolved from this
	// descriptor, so the directory the ownership check was made about is
	// the directory root acts in, whatever happens to the name meanwhile.
	root, err := job.confine()
	if err != nil {
		var named ruled
		if errors.As(err, &named) {
			return refuse(named.rule, "%s", named.message)
		}
		return refuse("helper-job-invalid", "%v", err)
	}
	defer root.Close()

	switch action {
	case ActionMount:
		reply = mount(job, root)
	case ActionUnmount:
		reply = unmount(job, root)
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

// unwind removes what was mounted and clears what the kernel left, so a
// failed mount leaves the machine as it found it.
//
// The second half matters as much as the first. The kernel creates its own
// directory inside the overlay's workdir, owned by root and unreadable,
// and only a privileged process can remove it -- so a failure that
// unmounted everything and left that behind would leave the user with
// residue they cannot clear, and with no record naming it either, because
// a rolled-back up forgets its record. That is being walled in by a
// command that reported a clean machine.
func unwind(job Job, root pathx.Root, made []string, reply Reply) Reply {
	reply.Stranded = rollback(made)
	reply.RolledBack = len(reply.Stranded) == 0
	if reply.RolledBack {
		if err := clearWork(job, root); err != nil {
			reply.Error += "\n\nand what the kernel left behind could not be " +
				"cleared: " + err.Error()
		}
	}
	return reply
}

// clearWork removes the kernel's leftover inside camp's work directory and
// gives the directory back to the invoking user.
func clearWork(job Job, root pathx.Root) error {
	if len(job.WorkParts) == 0 {
		return nil
	}
	work := filepath.Join(append([]string{root.Name()}, job.WorkParts...)...)
	if err := campsOwn(root, job.WorkParts, work); err != nil {
		return err
	}
	// Addressed from the root confine opened -- which the invoking user was
	// established to own, on that descriptor -- and by component, so that
	// root removing and giving away a tree cannot be redirected by a
	// symlink somewhere in it, nor by the base's own name being replaced
	// after the check. The components were checked to be plain names.
	area := fsx.In("work", root, job.WorkParts...)
	if err := area.RemoveTree("work"); err != nil {
		return err
	}
	if err := area.Chown(job.UID, job.GID); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// mount executes the plan, verifies it where it was built, and moves it.
//
// All three in one invocation, because sudo is exercised exactly once per
// command: giving the privilege back between mounting and moving and
// asking for it again would open a window in which a half-built tree
// exists and nothing is watching it.
func mount(job Job, root pathx.Root) Reply {
	reply := Reply{Version: JobVersion}
	var made []string

	// Every operand that exists is compared against what the front end
	// recorded before the first syscall that changes anything, so a job
	// whose operands have already moved is refused with the machine
	// untouched. It does not replace the comparison each mount makes a
	// moment before it happens -- that one is the authority, because it is
	// the one with no gap after it -- it only means the common case fails
	// early and cleanly.
	if err := precheck(job, root); err != nil {
		reply.Error = err.Error()
		if ruling, ok := err.(ruled); ok {
			reply.Rule = ruling.rule
		}
		// Through unwind although there is nothing to unwind: what it sets
		// is the reply's account of the machine, and this refusal happens
		// before the first syscall that changes anything. Returned bare, it
		// said the rollback had failed -- so an up that touched nothing
		// reported the workspace frozen, listed no mounts as still mounted,
		// and left a partial record for a composition that was never built.
		return unwind(job, root, nil, reply)
	}

	// The staging tree gets a private parent before anything is built in
	// it, or the move at the end cannot happen: MS_MOVE refuses a mount
	// whose parent is shared, and on a systemd machine everything on / is.
	// It is the first thing made and the last thing removed, so it is at
	// the bottom of the rollback list.
	staging := filepath.Join(append([]string{root.Name()}, job.StagingParts...)...)
	made, err := detach(root, job.StagingParts, staging, made)
	if err != nil {
		reply.Error = err.Error()
		return unwind(job, root, made, reply)
	}

	for _, operation := range job.Mounts {
		target, sourceFD, targetFD, ends, err := resolve(root, operation)
		if err != nil {
			reply.Error = err.Error()
			return unwind(job, root, made, reply)
		}

		mountable := operation.AsMount(target)
		// Both ends by descriptor, and no name resolved after them either:
		// the mount this makes is held by its own descriptor from the moment
		// it exists, so the read-only remount and the propagation change are
		// about it and not about a second resolution of the target's name.
		placed, err := mountx.MountByDescriptor(mountable, sourceFD, targetFD, ends)
		closeAll(sourceFD, targetFD)
		ends.Close()
		// Recorded the moment the mount exists, not when the whole
		// operation finishes: a bind that succeeded and a remount that did
		// not leaves a mount standing, and a rollback that skipped it would
		// report a clean machine that is not clean.
		if placed {
			made = append(made, target)
		}
		if err != nil {
			reply.Error = err.Error()
			return unwind(job, root, made, reply)
		}

		// What the mount answers as, read back from the root's own
		// descriptor, and the operation fails if it cannot be read.
		//
		// A dropped failure here produced a successful reply carrying a zero
		// identity, and a zero identity in a record is read by the teardown
		// as authority to unmount whatever stands at that path -- so the one
		// mount camp could not identify became the one mount camp would
		// remove without checking. The deliberate zero is the other one: a
		// teardown target whose record predates its mount, which job.go
		// documents, and that one is a record's silence rather than a
		// helper's.
		found, err := identityUnder(root, operation.TargetParts)
		if err != nil {
			reply.Rule = "helper-mount-unidentifiable"
			reply.Error = fmt.Sprintf("%s was mounted and camp could not read "+
				"back what it is: %v.\nA mount camp cannot identify is one a "+
				"teardown could not tell from a stranger's, so it is removed "+
				"again here rather than recorded as an identity of zero.", target, err)
			return unwind(job, root, made, reply)
		}
		reply.Results = append(reply.Results, Result{
			Target:  target,
			Outcome: "mounted",
			Device:  found.Device,
			Inode:   found.Inode,
		})
	}

	// The first verification pass, here rather than in the front end,
	// because the staging tree is where it has to be checked and the
	// privilege must not be given back in between.
	if problems := verifyStaging(job, root, staging); problems != "" {
		reply.Error = problems
		return unwind(job, root, made, reply)
	}

	// And the destination needs one too, for the other half of the same
	// kernel rule. Attaching a mount tree under a shared parent does not
	// merely propagate the attachment to that parent's peers -- it marks
	// the moved mounts themselves shared. Measured: a tree built private in
	// staging comes out shared at the live path, and making it private
	// afterwards is too late, because the copies in the peers were already
	// made and are not camp's to remove. A private parent means no
	// propagation happens at all.
	live := filepath.Join(append([]string{root.Name()}, job.LiveParts...)...)
	made, err = detach(root, job.LiveParts, live, made)
	if err != nil {
		reply.Error = err.Error()
		return unwind(job, root, made, reply)
	}

	// Both ends of the move are opened from the root the helper pinned,
	// following nothing, and the move is made through those descriptors: this is the
	// step that makes the tree visible to the machine, and a live directory
	// swapped between the verification and here would otherwise be root
	// attaching a verified composition wherever the swap pointed.
	stagingFD, err := root.Open(job.StagingParts, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		reply.Error = fmt.Sprintf("opening the staging tree %s again: %v", staging, err)
		return unwind(job, root, made, reply)
	}
	liveFD, err := root.Open(job.LiveParts, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		unix.Close(stagingFD)
		reply.Error = fmt.Sprintf("opening the composed tree's directory %s: %v", live, err)
		return unwind(job, root, made, reply)
	}
	err = mountx.Move(stagingFD, liveFD, staging, live)
	closeAll(stagingFD, liveFD)
	if err != nil {
		reply.Error = err.Error()
		return unwind(job, root, made, reply)
	}
	reply.Moved = true

	// The tree is at the live path now; what is left at the staging point
	// is the self-bind that made the move possible, over an empty
	// directory. It exists for the length of this one call and no record
	// mentions it, so it goes here.
	if outcome, err := mountx.Unmount(staging); outcome == mountx.Busy {
		reply.Stranded = append(reply.Stranded, staging)
		reply.Error = fmt.Sprintf("the composition is at %s, and the staging "+
			"mount point %s could not be removed: %v", live, staging, err)
	}
	return reply
}

// detach makes one directory its own private mount, addressed through the
// root the helper holds, and records it for rollback the moment a mount
// exists.
//
// The staging tree needs it before anything is built inside it and the
// live directory needs it before the tree is moved onto it, and both need
// the recording to happen on what mountx.Detach reports rather than on
// the whole call having succeeded: the self-bind and the propagation
// change are two syscalls, and a failure in the second leaves the first
// standing. Recorded only on success, that is a rollback reporting a
// clean machine over a mount camp made -- which is the one thing camp's
// failure handling may never do.
//
// The directory is opened from the pinned root, by component, following
// no symlink, and the descriptor is what is handed to the mount: nothing
// here resolves the staging or live name for the kernel to walk again.
func detach(root pathx.Root, parts []string, named string, made []string) ([]string, error) {
	fd, err := root.Open(parts, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return made, fmt.Errorf("opening %s to bind it onto itself: %w", named, err)
	}
	defer unix.Close(fd)

	standing, err := mountx.Detach(fd, named)
	if standing {
		made = append(made, named)
	}
	return made, err
}

// precheck compares what the job says about its operands with what is on
// the machine now, opening nothing that does not already exist.
func precheck(job Job, root pathx.Root) error {
	for _, operation := range job.Mounts {
		if operation.TargetIdent != "" {
			if err := compare(root, operation.TargetParts, operation.TargetIdent,
				"", operation.Target); err != nil {
				return err
			}
		}
		if operation.SourceIdent != "" {
			if err := compare(root, operation.SourceParts, operation.SourceIdent,
				operation.SourceType, operation.Source); err != nil {
				return err
			}
		}
		if operation.Kind != string(plan.Overlay) {
			continue
		}
		ends, err := resolveOperands(root, operation)
		if err != nil {
			return err
		}
		ends.Close()
	}
	return nil
}

// compare opens one operand beneath the pinned root and checks it against
// what the front end recorded: its identity always, and the kind of thing
// it is where the job carries one.
func compare(root pathx.Root, parts []string, identity, kind, path string) error {
	fd, err := root.Open(parts, unix.O_PATH)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer unix.Close(fd)
	if err := checkIdentity(fd, path, identity); err != nil {
		return err
	}
	return checkType(fd, path, kind)
}

// verifyStaging runs the path-based pass against the tree where it was
// built.
func verifyStaging(job Job, root pathx.Root, staging string) string {
	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		return err.Error()
	}

	built := plan.Plan{Live: staging}
	for _, operation := range job.Mounts {
		target := filepath.Join(append([]string{root.Name()}, operation.TargetParts...)...)
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
func resolve(root pathx.Root, operation JobMount) (string, int, int, mountx.Operands, error) {
	target := filepath.Join(append([]string{root.Name()}, operation.TargetParts...)...)
	ends := mountx.NoOperands()

	targetFD, err := root.Open(operation.TargetParts, unix.O_PATH)
	if err != nil {
		return "", -1, -1, ends, fmt.Errorf("opening the mount point %s: %w", target, err)
	}
	if err := checkIdentity(targetFD, target, operation.TargetIdent); err != nil {
		unix.Close(targetFD)
		return "", -1, -1, ends, err
	}

	if operation.Kind == string(plan.Overlay) {
		ends, err = resolveOperands(root, operation)
		if err != nil {
			unix.Close(targetFD)
			return "", -1, -1, mountx.NoOperands(), err
		}
		return target, -1, targetFD, ends, nil
	}

	if len(operation.SourceParts) == 0 {
		return target, -1, targetFD, ends, nil
	}
	sourceFD, err := root.Open(operation.SourceParts, unix.O_PATH)
	if err != nil {
		unix.Close(targetFD)
		return "", -1, -1, ends, fmt.Errorf("opening the mount source %s: %w", operation.Source, err)
	}
	if err := checkIdentity(sourceFD, operation.Source, operation.SourceIdent); err != nil {
		closeAll(sourceFD, targetFD)
		return "", -1, -1, ends, err
	}
	// And what kind of thing it is, read off the descriptor that is about
	// to be bound rather than off its name. The identity says it is the
	// same object; this says the object is what the front end took it for,
	// which is what decides whether the kernel will accept the bind at all.
	if err := checkType(sourceFD, operation.Source, operation.SourceType); err != nil {
		closeAll(sourceFD, targetFD)
		return "", -1, -1, ends, err
	}
	return target, sourceFD, targetFD, ends, nil
}

// insideStaging reports whether a target lies in the tree this job is
// building, which is the only place a mount point may legitimately not
// exist yet.
func insideStaging(job Job, parts []string) bool {
	if len(job.StagingParts) == 0 || len(parts) < len(job.StagingParts) {
		return false
	}
	for index, part := range job.StagingParts {
		if parts[index] != part {
			return false
		}
	}
	return true
}

// resolveOperands opens the overlay's three directories beneath the base
// and checks each against the identity the front end recorded. That every
// one of them carries an identity at all was settled by confine, before
// the helper's first syscall.
//
// The composed tree decides what the whole composition shows and where
// every write lands. Until this existed it was the one operation whose
// operands crossed to root as bare paths, resolved again by the kernel at
// mount time -- so a component the user owns, replaced in between, was a
// composition mounted from somewhere else entirely and verified as
// sound, because what was verified was the replacement.
func resolveOperands(root pathx.Root, operation JobMount) (mountx.Operands, error) {
	ends := mountx.NoOperands()
	open := func(parts []string, identity, path, what string) (int, error) {
		fd, err := root.Open(parts, unix.O_PATH|unix.O_DIRECTORY)
		if err != nil {
			return -1, fmt.Errorf("opening the overlay's %s %s: %w", what, path, err)
		}
		if err := checkIdentity(fd, path, identity); err != nil {
			unix.Close(fd)
			return -1, err
		}
		return fd, nil
	}

	for index, parts := range operation.LowerParts {
		fd, err := open(parts, identityAt(operation.LowerIdents, index),
			operation.Lower[index], "lower layer")
		if err != nil {
			ends.Close()
			return mountx.NoOperands(), err
		}
		ends.Lower = append(ends.Lower, fd)
	}
	if operation.Upper == "" {
		return ends, nil
	}
	upper, err := open(operation.UpperParts, operation.UpperIdent, operation.Upper, "upper layer")
	if err != nil {
		ends.Close()
		return mountx.NoOperands(), err
	}
	ends.Upper = upper
	work, err := open(operation.WorkParts, operation.WorkIdent, operation.Work, "work directory")
	if err != nil {
		ends.Close()
		return mountx.NoOperands(), err
	}
	ends.Work = work
	return ends, nil
}

func identityAt(identities []string, index int) string {
	if index >= len(identities) {
		return ""
	}
	return identities[index]
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
		// Only a mount point inside the staging tree reaches here without
		// one, and only because it did not exist when the job was built.
		// Every other operand is refused before it is opened.
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

// checkType refuses when the object opened is not the kind of thing the
// front end saw.
//
// A bind puts one object over another and the kernel refuses to put a
// directory on a file or a file on a directory, so the kind is part of
// what makes an operand the operand camp planned. It is read from the
// descriptor that is about to be used, which is the only reading with no
// gap after it: the name could be a directory when the front end looked,
// a file by now, and a third thing by the time a second stat asked.
//
// An empty expectation means the job carries no kind for this operand --
// the overlay's own operands and every mount point -- and there is
// nothing to compare. checkable has already refused a source that arrived
// without one.
func checkType(fd int, path, expected string) error {
	if expected == "" {
		return nil
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("looking at %s: %w", path, err)
	}
	got := pathx.TypeOf(st.Mode)
	if got != pathx.Dir && got != pathx.File {
		return fmt.Errorf("%s is a %s, and camp binds directories and regular "+
			"files.\nNothing has been mounted", path, got)
	}
	if string(got) != expected {
		return fmt.Errorf("%s is a %s and camp checked a %s.\nA bind puts one "+
			"kind of thing over another and the kernel refuses to mix the two, "+
			"so something replaced this between the check and the mount. "+
			"Nothing has been mounted", path, got, expected)
	}
	return nil
}

// standsThere reports why a target must not be unmounted, or "" when it
// may be.
//
// The first question is whether anything is mounted there at all, and it
// has to be asked of the mount table rather than of the path. A path
// whose mount is gone resolves to whatever was underneath it -- for the
// live directory, the empty directory the composition stood on -- whose
// device and inode are of course not the composition's. Comparing those
// reads a finished job as a stranger's mount: measured, after a teardown
// that unmounted everything and did not get to say so, where 'camp down'
// then refused to remove eleven mounts that were already gone.
//
// Four cases pass: nothing is mounted there, so the unmount will answer
// "absent"; the record carries no identity, so there is nothing to
// compare and refusing would wall somebody in behind mounts camp made;
// the path cannot be looked at, which the unmount itself will answer for;
// and the identity matches.
//
// What is compared comes from a descriptor resolved beneath the pinned
// root, from the target's own components, and not from a second
// resolution of the recorded path. The path is still what says which
// mount table entry this is about, because a mount table is a table of
// paths.
func standsThere(table []mountinfo.Entry, root pathx.Root, parts []string, target JobTarget) string {
	if target.Device == 0 && target.Inode == 0 {
		return ""
	}
	if len(mountinfo.At(table, target.Path)) == 0 {
		return ""
	}
	found, err := identityUnder(root, parts)
	if err != nil {
		return ""
	}
	if found.Device == target.Device && found.Inode == target.Inode {
		return ""
	}
	return fmt.Sprintf("%s is %d:%d and camp mounted %d:%d there. Something "+
		"else stands at that path now, and this helper will not unmount it: "+
		"camp's own mount is gone, and what is there belongs to somebody else.",
		target.Path, found.Device, found.Inode, target.Device, target.Inode)
}

type identity struct {
	Device uint64
	Inode  uint64
}

// identityUnder answers for the object standing at parts below the root,
// opened from the root's own descriptor and following no symlink.
//
// Below a mount point it is the mounted object that answers, which is
// what the caller is asking about: a mount's identity is the identity of
// what it put there.
func identityUnder(root pathx.Root, parts []string) (identity, error) {
	fd, err := root.Open(parts, unix.O_PATH)
	if err != nil {
		return identity{}, err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
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
func unmount(job Job, root pathx.Root) Reply {
	reply := Reply{Version: JobVersion}
	for _, target := range job.Targets {
		// The recorded path written as components beneath the pinned root,
		// once, so that the identity check and everything after it start at
		// the descriptor confine opened rather than at the string. confine
		// has already refused a target that is not beneath the base, so this
		// only fails on a job that changed underneath us, and then the target
		// is stepped over rather than acted on.
		parts, err := componentsBeneath(root, target.Path)
		if err != nil {
			reply.Results = append(reply.Results, Result{
				Target:  target.Path,
				Outcome: "mismatch",
				Error:   err.Error(),
			})
			continue
		}

		// Identity before the syscall. A recorded path proves nothing on its
		// own: camp's mount may have gone and somebody else's may stand at
		// the same name, and removing that one would be root unmounting a
		// stranger's mount because camp wrote the path down once. The
		// mismatch is reported and stepped over rather than failing the
		// whole teardown, because the rest of the composition still has to
		// come down.
		//
		// The table is read again for each target, because every unmount
		// changes it: a stale one would say a path is still mounted after
		// this job removed it.
		table, err := mountinfo.Read(mountinfo.Self)
		if err != nil {
			reply.Error = err.Error()
			return reply
		}
		if mismatch := standsThere(table, root, parts, target); mismatch != "" {
			reply.Results = append(reply.Results, Result{
				Target:  target.Path,
				Outcome: "mismatch",
				Error:   mismatch,
			})
			continue
		}
		// Still open, and deliberately: the identity is now decided on a
		// descriptor resolved beneath the pinned root, and the unmount itself
		// still hands the kernel a path it resolves again. umount2 takes a
		// path, and the descriptor-safe form of it -- umount2 on the
		// descriptor's own /proc/self/fd name -- depends on kernel behaviour
		// nothing here has measured. Closing this needs that primitive
		// measured first, in a namespace or a disposable machine; until then
		// the check is on the descriptor and the act is on the name, and
		// there is a window between them.
		outcome, err := mountx.Unmount(target.Path)
		result := Result{Target: target.Path, Outcome: string(outcome)}
		if err != nil && outcome == mountx.Busy {
			result.Error = err.Error()
			reply.Stranded = append(reply.Stranded, target.Path)
		}
		reply.Results = append(reply.Results, result)
	}

	// The one thing the kernel creates as root: the overlay's leftover
	// work directory, mode 000. It is removed here, while there is still
	// the privilege to do it, so that the user is never left with a
	// directory of camp's that they cannot clear.
	if len(reply.Stranded) == 0 {
		if err := clearWork(job, root); err != nil {
			reply.Error = err.Error()
			var named ruled
			if errors.As(err, &named) {
				reply.Rule = named.rule
			}
		}
	}
	return reply
}
