package privileged

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

	// One graveyard for the whole invocation, and nothing made until a
	// mount actually has to come down. Every unmount the helper performs
	// goes through it: the mount is named by a descriptor, moved into a
	// directory only root can reach, and unmounted there, so the name the
	// kernel finally resolves is not one the invoking user can replace.
	// mountx's own file carries the measurements that shape it and says
	// which mounts it cannot help.
	grave := mountx.NewGraveyard()

	switch action {
	case ActionMount:
		reply = mount(job, root, grave)
	case ActionUnmount:
		reply = unmount(job, root, grave)
	default:
		reply.Error = fmt.Sprintf("unknown action %q", action)
	}

	// Before the reply is built, because what it could not clear is part of
	// what this invocation left on the machine.
	if err := grave.Close(); err != nil {
		reply.Error = strings.TrimSpace(reply.Error + "\n\n" + err.Error())
	}

	code := 0
	if reply.Error != "" || len(reply.Stranded) > 0 {
		code = 1
	}
	barrier(job, "reply-encoded")
	return finish(out, reply, code)
}

// refused puts a ruled refusal into a reply that already carries results.
func refused(reply Reply, err error) Reply {
	reply.Error = err.Error()
	var named ruled
	if errors.As(err, &named) {
		reply.Rule = named.rule
	}
	return reply
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
func unwind(job Job, root pathx.Root, made []place, reply Reply,
	grave *mountx.Graveyard) Reply {
	// Said once, and only when there is something to unwind: a rollback
	// removes camp's own mounts by descriptor through the graveyard, and on
	// a machine where that cannot be made it removes them by name instead.
	// It does remove them -- a rollback that refused to act because it
	// could not act perfectly would leave a half-built composition standing
	// -- and a reply that did not say which route it took would be claiming
	// a guarantee this run did not have.
	if len(made) > 0 {
		if err := grave.Open(); err != nil {
			reply.Error = strings.TrimSpace(reply.Error) + "\n\nWhat camp had " +
				"mounted was removed by name rather than by descriptor: " + err.Error()
		}
	}
	reply.Stranded = rollback(root, made, grave)
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
func mount(job Job, root pathx.Root, grave *mountx.Graveyard) Reply {
	reply := Reply{Version: JobVersion}
	var made []place

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
		return unwind(job, root, nil, reply, grave)
	}
	barrier(job, "prechecked")

	// The staging tree gets a private parent before anything is built in
	// it, or the move at the end cannot happen: MS_MOVE refuses a mount
	// whose parent is shared, and on a systemd machine everything on / is.
	// It is the first thing made and the last thing removed, so it is at
	// the bottom of the rollback list.
	staging := filepath.Join(append([]string{root.Name()}, job.StagingParts...)...)
	made, err := detach(root, job.StagingParts, staging, made)
	if err != nil {
		reply.Error = err.Error()
		return unwind(job, root, made, reply, grave)
	}
	barrier(job, "staging-bound")

	for _, operation := range job.Mounts {
		target, sourceFD, targetFD, ends, err := resolve(root, operation)
		if err != nil {
			reply.Error = err.Error()
			return unwind(job, root, made, reply, grave)
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
			made = append(made, place{parts: operation.TargetParts, path: target})
		}
		if err != nil {
			reply.Error = err.Error()
			return unwind(job, root, made, reply, grave)
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
			return unwind(job, root, made, reply, grave)
		}
		reply.Results = append(reply.Results, Result{
			Target:  target,
			Outcome: "mounted",
			Device:  found.Device,
			Inode:   found.Inode,
		})
		barrier(job, "mount-made")
	}

	// The first verification pass, here rather than in the front end,
	// because the staging tree is where it has to be checked and the
	// privilege must not be given back in between.
	if problems := verifyStaging(job, root, staging); problems != "" {
		reply.Error = problems
		return unwind(job, root, made, reply, grave)
	}
	barrier(job, "staging-verified")

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
		return unwind(job, root, made, reply, grave)
	}
	barrier(job, "live-bound")

	// Both ends of the move are opened from the root the helper pinned,
	// following nothing, and the move is made through those descriptors: this is the
	// step that makes the tree visible to the machine, and a live directory
	// swapped between the verification and here would otherwise be root
	// attaching a verified composition wherever the swap pointed.
	barrier(job, "before-move-open")
	stagingFD, err := root.Open(job.StagingParts, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		reply.Error = fmt.Sprintf("opening the staging tree %s again: %v", staging, err)
		return unwind(job, root, made, reply, grave)
	}
	liveFD, err := root.Open(job.LiveParts, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		unix.Close(stagingFD)
		reply.Error = fmt.Sprintf("opening the composed tree's directory %s: %v", live, err)
		return unwind(job, root, made, reply, grave)
	}
	err = mountx.Move(stagingFD, liveFD, staging, live)
	closeAll(stagingFD, liveFD)
	if err != nil {
		reply.Error = err.Error()
		return unwind(job, root, made, reply, grave)
	}
	reply.Moved = true
	barrier(job, "moved")

	// The tree is at the live path now; what is left at the staging point
	// is the self-bind that made the move possible, over an empty
	// directory. It exists for the length of this one call and no record
	// mentions it, so it goes here -- through the same route as everything
	// else the helper takes down, which for a self-bind means the name,
	// because a mount whose parent is shared cannot be moved anywhere.
	standing, err := held(root, job.StagingParts, staging)
	if err != nil {
		reply.Stranded = append(reply.Stranded, staging)
		reply.Error = fmt.Sprintf("the composition is at %s, and the staging "+
			"mount point %s could not be opened to be removed: %v", live, staging, err)
	} else {
		outcome, failed := grave.Remove(&standing)
		standing.Close()
		if outcome == mountx.Busy {
			reply.Stranded = append(reply.Stranded, staging)
			reply.Error = fmt.Sprintf("the composition is at %s, and the staging "+
				"mount point %s could not be removed: %v", live, staging, failed)
		}
	}
	barrier(job, "staging-unbound")
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
func detach(root pathx.Root, parts []string, named string, made []place) ([]place, error) {
	fd, err := root.Open(parts, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return made, fmt.Errorf("opening %s to bind it onto itself: %w", named, err)
	}
	defer unix.Close(fd)

	standing, err := mountx.Detach(fd, named)
	if standing {
		made = append(made, place{parts: parts, path: named})
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
// It is asked of the descriptor the caller opened, and of nothing else.
// That descriptor is what the removal is then performed on, so the object
// this compares and the object that comes down are one object rather than
// one name resolved twice. The caller has already established that
// something is mounted at the recorded path, which is the question only
// the mount table can answer.
//
// Two cases pass without a comparison: the record carries no identity, so
// there is nothing to compare and refusing would wall somebody in behind
// mounts camp made; and the identity matches. A record with no identity is
// what a run killed before its first reply leaves behind, and the two
// self-binds never have one.
//
// A mount whose identity is read as something else is left standing and
// reported, because camp's own mount is then gone and what is there
// belongs to somebody else.
func standsThere(fd int, target JobTarget) string {
	if target.Device == 0 && target.Inode == 0 {
		return ""
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return ""
	}
	found := identity{Device: uint64(st.Dev), Inode: st.Ino}
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

// place is one mount camp made: the components it was addressed by
// beneath the pinned root, and the name a person reads in a message.
//
// The components are what the rollback needs. A mount recorded only as a
// path would have to be resolved again to be taken down, and the walk that
// resolved it the first time is the one thing that established it is the
// object camp made.
type place struct {
	parts []string
	path  string
}

// rollback removes what was mounted, in reverse, and returns what it
// could not remove.
//
// Each one is opened beneath the root the helper pinned, from the
// components it was mounted through, and handed to the graveyard as a
// descriptor -- the same route the teardown takes, for the same reason.
// A mount whose place cannot be opened at all is stranded rather than
// guessed at: root has nothing left to identify it by, and unmounting a
// name in that state is unmounting whatever the name reaches now.
func rollback(root pathx.Root, made []place, grave *mountx.Graveyard) []string {
	var stranded []string
	for index := len(made) - 1; index >= 0; index-- {
		standing, err := held(root, made[index].parts, made[index].path)
		if err != nil {
			stranded = append(stranded, made[index].path)
			continue
		}
		outcome, _ := grave.Remove(&standing)
		standing.Close()
		if outcome == mountx.Busy {
			stranded = append(stranded, made[index].path)
		}
	}
	return stranded
}

// held opens one mount beneath the pinned root, out of a single walk.
//
// The identity check, the handle that removes it and the way back are all
// made on what this returns, so they are all about one object. Two walks
// would be two resolutions of one name, and everything the helper does
// after confine exists so that there is one.
func held(root pathx.Root, parts []string, path string) (mountx.Mounted, error) {
	none := mountx.Mounted{FD: -1, Dir: -1}
	if len(parts) == 0 {
		return none, fmt.Errorf("%s is the environment root itself, and this "+
			"helper takes down what a composition mounted inside it", path)
	}
	fd, dir, name, err := root.OpenIn(parts, unix.O_PATH)
	if err != nil {
		return none, err
	}
	return mountx.Mounted{FD: fd, Dir: dir, Name: name, Path: path}, nil
}

// whereTheBaseIs refuses when the environment root has moved since the job
// was built.
//
// The teardown asks the mount table which of its recorded paths still have
// something at them, and the table answers about paths. A mount point
// cannot be renamed while it is mounted -- C34 -- but an ancestor can, and
// the invoking user owns every one of them: rename the environment root
// and the kernel reports every camp mount under the new name, while the
// job still names the old one. Every remaining target then reads "nothing
// is mounted there", which is a true answer to a question about a name and
// a false one about the machine.
//
// Measured, by the rename race at stands-there: 'camp down' removed the
// first target, the name was swapped, and it reported the remaining five
// absent and exited 0 -- so the front end released the record while camp's
// own composition stood, and no record was left to take it apart with.
//
// The descriptor is what answers. It cannot be redirected and it knows
// where it is, so comparing where it is against where the job says it
// should be costs one readlink and closes the whole class. A teardown that
// finds them different stops with nothing unmounted: the record is kept,
// and putting the directory back where the record names it makes 'camp
// down' work again.
func whereTheBaseIs(job Job, root pathx.Root) error {
	now, err := root.Current()
	if err != nil {
		return refuse("helper-base-unreadable",
			"camp cannot tell where the environment root it opened is now: %v.\n"+
				"A teardown compares the mount table's paths against the ones the "+
				"record names, and that comparison is only worth making while "+
				"those are the same directory. Nothing has been unmounted.", err)
	}
	if now == job.Base {
		return nil
	}
	return refuse("helper-base-renamed",
		"this job is for %s and the directory this helper opened is now at "+
			"%s.\nSomething renamed it after the job was built. The kernel "+
			"reports every mount at the path it is at now, so every path in this "+
			"record would answer \"nothing is mounted there\" -- and a teardown "+
			"that reported itself finished on that would leave this "+
			"composition standing with nothing left to take it apart with.\n"+
			"Nothing has been unmounted and the record is unchanged. Put the "+
			"directory back at %s and run 'camp down' again.",
		job.Base, now, job.Base)
}

// unmount removes the recorded targets, in the order given.
//
// It never detaches anything lazily. A mount it cannot remove stays
// mounted, is reported as still mounted, and makes the command fail.
func unmount(job Job, root pathx.Root, grave *mountx.Graveyard) Reply {
	reply := Reply{Version: JobVersion}
	for _, target := range job.Targets {
		// The recorded path written as components beneath the pinned root,
		// once, so that the identity check and everything after it start at
		// the descriptor confine opened rather than at the string. confine
		// has already refused a target that is not beneath the base, so this
		// only fails on a job that changed underneath us, and then the target
		// is stepped over rather than acted on.
		parts, err := componentsBeneath(root, target.Path)
		if err == nil && len(parts) == 0 {
			err = fmt.Errorf("%s is the environment root itself, and this "+
				"helper takes down what a composition mounted inside it",
				target.Path)
		}
		if err != nil {
			reply.Results = append(reply.Results, Result{
				Target:  target.Path,
				Outcome: "mismatch",
				Error:   err.Error(),
			})
			continue
		}

		// The table is read again for each target, because every unmount
		// changes it: a stale one would say a path is still mounted after
		// this job removed it.
		table, err := mountinfo.Read(mountinfo.Self)
		if err != nil {
			reply.Error = err.Error()
			return reply
		}

		// Asked with every read of the table, because the rename can happen
		// at any point in this loop and everything below reads that table by
		// path. A rename between this check and the answer below it costs one
		// target read as absent, and the next turn of the loop stops the job
		// -- with the record kept, which is what makes that recoverable.
		if err := whereTheBaseIs(job, root); err != nil {
			return refused(reply, err)
		}

		// Whether anything is mounted there at all is the first question,
		// and only the table can answer it. A path whose mount is gone
		// resolves to whatever was underneath -- for the live directory, the
		// empty directory the composition stood on -- and every recorded
		// place is empty in one of the two states the record covers. This is
		// the answer that costs no syscall at all.
		if len(mountinfo.At(table, target.Path)) == 0 {
			reply.Results = append(reply.Results, Result{
				Target:  target.Path,
				Outcome: string(mountx.Absent),
			})
			continue
		}

		// One walk, and everything after it is about what that walk opened:
		// the identity is read off this descriptor, the handle that removes
		// the mount is taken from it, and the directory it stands in comes
		// out of the same walk so a mount that will not come down can be put
		// back without resolving anything.
		//
		// A mount the table names at a path this cannot reach beneath the
		// pinned root is refused rather than unmounted. That is the shape of
		// the attack: an ancestor renamed away between the record and now,
		// so the name still reaches a mount and no longer reaches camp's.
		standing, err := held(root, parts, target.Path)
		if err != nil {
			reply.Results = append(reply.Results, Result{
				Target:  target.Path,
				Outcome: "mismatch",
				Error: fmt.Sprintf("something is mounted at %s and camp cannot "+
					"reach that path from the environment root it opened: %v.\n"+
					"This helper unmounts what it can identify, and it will not "+
					"hand the kernel a name instead", target.Path, err),
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
		if mismatch := standsThere(standing.FD, target); mismatch != "" {
			standing.Close()
			reply.Results = append(reply.Results, Result{
				Target:  target.Path,
				Outcome: "mismatch",
				Error:   mismatch,
			})
			continue
		}

		// The teardown will not act without the graveyard. Nothing has been
		// touched at this point, so refusing costs the person a message and
		// a second 'camp down'; acting without it would mean root resolving
		// a name inside a directory whose owner is the one person this
		// helper defends against. The rollback's answer is the other one,
		// and unwind says why.
		if err := grave.Open(); err != nil {
			standing.Close()
			reply.Rule = "helper-no-graveyard"
			reply.Error = fmt.Sprintf("camp takes a mount somewhere only root "+
				"can name before it unmounts it, and it could not make that "+
				"place: %v.\nNothing has been unmounted and the record is "+
				"unchanged. This is a fault of the machine rather than of the "+
				"composition: %s has to be a directory root can create in.",
				err, mountx.GraveyardBase)
			return reply
		}

		barrier(job, "stands-there")
		outcome, err := grave.Remove(&standing)
		standing.Close()
		result := Result{Target: target.Path, Outcome: string(outcome)}
		if err != nil && outcome == mountx.Busy {
			result.Error = err.Error()
			reply.Stranded = append(reply.Stranded, target.Path)
		}
		reply.Results = append(reply.Results, result)
	}

	// And once more after the last target, which the loop's own check
	// cannot cover: a rename during the final unmount would otherwise be
	// reported as a teardown that finished.
	if err := whereTheBaseIs(job, root); err != nil {
		return refused(reply, err)
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
