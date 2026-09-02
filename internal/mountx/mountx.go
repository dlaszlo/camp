// Package mountx performs a plan, and takes a failed one apart again.
//
// Deciding what to mount and mounting it are separate responsibilities.
// The plan package decides; this one acts, and it acts by syscall, from
// inside the namespace camp created. There is no shelling out to mount(8)
// anywhere: the messages it prints are translated, its exit codes are
// coarse, and the plan is already a data structure that does not need
// re-serialising into command-line syntax to be executed.
//
// Two facts shape every operation here.
//
// MS_BIND|MS_RDONLY in one mount(2) call silently ignores the read-only
// flag. A read-only bind is therefore always two calls, and the result is
// never trusted -- verification inspects the mount afterwards rather than
// believing the call.
//
// A read-only remount inside a user namespace must preserve the source
// mount's locked flags -- nosuid, nodev, noexec and the atime family --
// or the kernel refuses it with EPERM. That is what makes a composition
// under a nosuid,nodev filesystem such as tmpfs work at all: the failure
// there is a missing OR in the remount, not a property of the
// filesystem. The flags are read from the mount the bind just created, so
// what is replicated is what the kernel actually has rather than what
// anybody assumed.
//
// What the kernel is told about the composed tree is derived once, in
// one object, and the mount is performed from it. The verification and
// 'camp status' compare a mounted overlay against it, and 'camp plan'
// prints it. What that replaced was several renderings of the same
// operands -- the fsconfig calls, an expectation composed inside the
// verification, the printed call sequence -- none of which was ever
// compared with any other, so a change to one of them was a check or a
// plan describing a mount nobody made.
//
// MNT_DETACH appears nowhere in this package and nowhere in camp. A lazy
// unmount removes a mount from the table while it is still alive and
// still being written through -- measured -- and a rollback that used one
// would report a clean namespace over a mount something is still writing
// through. It was --force wearing another name.
//
// And camp does not believe any of these calls in any case. Verification
// runs after the mounts and inspects the mount table and the tree itself,
// so a flag that did not take or a mount that landed somewhere else comes
// back as a refusal and a rollback -- the same way the read-only bind that
// MS_BIND silently ignores has always been caught.
package mountx

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/plan"
)

// Outcome is what happened to one unmount.
type Outcome string

const (
	// Unmounted is the only good outcome.
	Unmounted Outcome = "unmounted"
	// Absent means it was not a mount point in the first place.
	Absent Outcome = "absent"
	// Busy means something is holding it. It is an error, reported with
	// the holder named -- never quietly turned into a success.
	Busy Outcome = "busy"
)

// OverlayConfig is everything the kernel is told about one composed
// tree: which filesystem to make, and the ordered fsconfig calls that
// fill its context before it is created.
//
// It exists because several things have to say the same thing about one
// mount and used to say it several times. The mount performed one
// fsconfig per operand; the verification composed its own expectation out
// of the same plan fields; the printed plan rendered a third; and nothing
// compared any two of them. An operand added, reordered or renamed in one
// of them was a check or a plan describing a mount that was made
// differently, with every test still green.
//
// So this is the one derivation, and the mount is performed *from* it.
// Changing what the kernel is sent is changing this object, which the
// verification and 'camp status' compare against the mounted filesystem.
// There is no side left to drift from.
type OverlayConfig struct {
	// FSType is the filesystem asked of fsopen, and what the mount has to
	// answer as afterwards.
	FSType string
	// Steps are the fsconfig calls, in the order the kernel is given
	// them. The order is load-bearing for the lower layers: "lowerdir+"
	// appends one layer, so these calls are the layer order, leftmost
	// first.
	Steps []Fsconfig
}

// Fsconfig is one call that fills an overlay's filesystem context.
type Fsconfig struct {
	// Key is what the kernel is given: "lowerdir+" for one lower layer,
	// "upperdir", "workdir", or the name of a flag.
	Key string
	// Path is the directory whose descriptor carries Key, and it is the
	// one thing that says which kind of call this is: a flag has no
	// operand, so it has no path, and Flag below is that question asked
	// in one place.
	Path string
	// Operand and Index say which of the opened directories carries Path.
	// They are how the executor finds the descriptor and mean nothing for
	// a flag.
	Operand OperandKind
	Index   int
}

// Flag reports whether this call sets a flag by name rather than giving
// the kernel a descriptor.
func (f Fsconfig) Flag() bool { return f.Path == "" }

// what names the operand for a message, in the words the plan uses.
func (f Fsconfig) what() string {
	switch f.Operand {
	case OperandLower:
		return "lower layer"
	case OperandUpper:
		return "upper layer"
	case OperandWork:
		return "work directory"
	}
	return f.Key
}

// OperandKind says which of an overlay's opened directories one step is
// given.
type OperandKind string

const (
	// OperandFlag is a step with no operand at all.
	OperandFlag  OperandKind = ""
	OperandLower OperandKind = "lower"
	OperandUpper OperandKind = "upper"
	OperandWork  OperandKind = "work"
)

// OverlayFS is what an overlay answers as, to fsopen and to statfs
// afterwards.
const OverlayFS = "overlay"

// DescribeOverlay derives what the kernel will be told about one planned
// overlay.
//
// The one derivation. Everything that has an opinion about this mount --
// the syscalls, the verification, the printed plan -- reads it from here,
// so an operand added or reordered here is added or reordered in all of
// them at once.
func DescribeOverlay(m plan.Mount) OverlayConfig {
	described := OverlayConfig{FSType: OverlayFS}
	for index, lower := range m.Lower {
		// "lowerdir+" appends one layer, so the order of these calls is the
		// order of the layers, leftmost first.
		described.Steps = append(described.Steps, Fsconfig{
			Key: "lowerdir+", Path: lower, Operand: OperandLower, Index: index})
	}
	if m.Upper != "" {
		described.Steps = append(described.Steps,
			Fsconfig{Key: "upperdir", Path: m.Upper, Operand: OperandUpper},
			Fsconfig{Key: "workdir", Path: m.Work, Operand: OperandWork})
	}
	if m.Xattr != "" {
		described.Steps = append(described.Steps, Fsconfig{Key: m.Xattr})
	}
	return described
}

// Operands are an overlay's directories, opened before the mount is made.
//
// The composed tree is mounted through the kernel's mount API -- fsopen,
// fsconfig, fsmount, move_mount -- rather than by handing mount(2) an
// option string, and this is why. An option string names the operands by
// path, and the kernel resolves those paths at mount time, following
// whatever symlinks are there then: the directory somebody checked and
// the directory that gets mounted need not be the same one. A descriptor
// names the object it was opened on and nothing else.
//
// The obvious alternative was measured and rejected. mount(2) accepts
// /proc/self/fd/N as an operand and mounts the right object -- and then
// records those strings in the kernel's table for the life of the mount,
// so /proc/self/mountinfo says lowerdir=/proc/self/fd/6 and nothing
// afterwards, camp's own verification included, can see what was mounted.
// The mount API records the real paths (measured: lowerdir+=<the real
// directory>), which is what a person reading /proc/mounts needs too.
type Operands struct {
	// Lower are the read-only layers, in the order they are given to the
	// kernel: leftmost wins.
	Lower []int
	// Upper and Work are the writable layer and its work directory, or -1
	// when the overlay has no upper and is therefore read-only.
	Upper int
	Work  int
}

// NoOperands is the empty set, with the two optional descriptors marked
// absent rather than left as descriptor zero.
func NoOperands() Operands { return Operands{Upper: -1, Work: -1} }

// For returns the descriptor one described step is performed with.
//
// A step whose operand was never opened is an error rather than a
// descriptor picked out of the zero value: descriptor 0 is this
// process's standard input, and giving the kernel that as a lower layer
// is a mount made of something nobody named.
func (o Operands) For(step Fsconfig) (int, error) {
	switch step.Operand {
	case OperandLower:
		if step.Index < len(o.Lower) {
			return o.Lower[step.Index], nil
		}
	case OperandUpper:
		if o.Upper >= 0 {
			return o.Upper, nil
		}
	case OperandWork:
		if o.Work >= 0 {
			return o.Work, nil
		}
	}
	return -1, fmt.Errorf("the overlay's %s %s was never opened, so there is "+
		"no descriptor to give the kernel for %s", step.what(), step.Path, step.Key)
}

// hold puts one opened operand where the step that named it will find it.
func (o *Operands) hold(step Fsconfig, fd int) {
	switch step.Operand {
	case OperandLower:
		o.Lower = append(o.Lower, fd)
	case OperandUpper:
		o.Upper = fd
	case OperandWork:
		o.Work = fd
	}
}

// Close gives back every descriptor an Operands holds.
func (o Operands) Close() {
	for _, fd := range o.Lower {
		unix.Close(fd)
	}
	if o.Upper >= 0 {
		unix.Close(o.Upper)
	}
	if o.Work >= 0 {
		unix.Close(o.Work)
	}
}

// OpenOperands opens an overlay's operands by path.
//
// By path, because the caller resolved them itself and is mounting in its
// own namespace: there is no privilege boundary between the check and the
// mount, and the paths came out of a plan that refuses a repository which
// is a symlink.
func OpenOperands(m plan.Mount) (Operands, error) {
	ends := NoOperands()
	// Opened from the description the mount is performed from, so the set
	// that is opened and the set that is given to the kernel are one list
	// read twice and not two lists.
	for _, step := range DescribeOverlay(m).Steps {
		if step.Flag() {
			continue
		}
		fd, err := unix.Open(step.Path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			ends.Close()
			return NoOperands(), fmt.Errorf("opening the %s %s: %w",
				step.what(), step.Path, err)
		}
		ends.hold(step, fd)
	}
	return ends, nil
}

// Overlay creates the composed tree from its opened operands and attaches
// it to an opened mount point.
//
// Every step is one syscall of the mount API: fsopen makes a filesystem
// context, fsconfig fills it -- each directory as a descriptor, never as
// a name -- fsmount turns it into a mount attached to nothing, and
// move_mount puts that mount where it belongs. Nothing between those
// steps resolves a path.
func Overlay(m plan.Mount, ends Operands, target int) error {
	mounted, err := overlayMount(m, ends)
	if err != nil {
		return err
	}
	defer unix.Close(mounted)

	if err := unix.MoveMount(mounted, "", target, "",
		unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); err != nil {
		return fmt.Errorf("attaching the overlay to %s: %w", m.Target, err)
	}
	return nil
}

// overlayMount is the first four of those steps on their own: the
// composed tree as a mount attached to nothing, held by the descriptor
// fsmount returned.
func overlayMount(m plan.Mount, ends Operands) (int, error) {
	described := DescribeOverlay(m)
	context, err := unix.Fsopen(described.FSType, unix.FSOPEN_CLOEXEC)
	if err != nil {
		return -1, fmt.Errorf("asking this kernel for an overlay filesystem: %w.\n"+
			"camp mounts the composed tree through the kernel's mount API "+
			"(fsopen, fsconfig, fsmount, move_mount) so that every layer is a "+
			"descriptor rather than a name something else could redirect. A "+
			"kernel without that API cannot be given the guarantee", err)
	}
	defer unix.Close(context)

	if err := fill(context, described, ends); err != nil {
		return -1, err
	}
	if err := unix.FsconfigCreate(context); err != nil {
		return -1, fmt.Errorf("creating the overlay for %s: %w", m.Target, err)
	}

	mounted, err := unix.Fsmount(context, unix.FSMOUNT_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("turning the overlay into a mount: %w", err)
	}
	return mounted, nil
}

// fill performs the described calls against a filesystem context, in
// order.
//
// The description is the call sequence. Nothing here decides what to
// send: an operand or a flag reaches the kernel because it is in that
// list, which is the same list the verification compares against the
// mounted filesystem. It is a function of its own
// so that a test can hold the calls camp makes against the description
// they are supposed to be performing.
func fill(context int, described OverlayConfig, ends Operands) error {
	for _, step := range described.Steps {
		if err := configure(context, step, ends); err != nil {
			return err
		}
	}
	return nil
}

// configure performs one described step against a filesystem context.
func configure(context int, step Fsconfig, ends Operands) error {
	if step.Flag() {
		if err := fsconfigFlag(context, step.Key); err != nil {
			return fmt.Errorf("asking the overlay for %s: %w", step.Key, err)
		}
		return nil
	}
	fd, err := ends.For(step)
	if err != nil {
		return err
	}
	if err := fsconfigFd(context, step.Key, fd); err != nil {
		return fmt.Errorf("giving the overlay its %s %s: %w", step.what(), step.Path, err)
	}
	return nil
}

// fsconfigFd and fsconfigFlag are the two calls that fill a filesystem
// context.
//
// Variables, so that a test can record the sequence camp sends the
// kernel and hold it against the description it is supposed to be
// performing. There is no other way to observe it: a real context comes
// from fsopen, and nothing in this repository may mount. They exist for
// the test, the way doctorTable does in internal/cli and afterTypeCheck
// in internal/fsx, and a running camp never replaces them.
var (
	fsconfigFd   = unix.FsconfigSetFd
	fsconfigFlag = unix.FsconfigSetFlag
)

// Mount performs one operation and leaves it private.
//
// It reports whether a mount now exists at the target, which is a
// different question from whether the operation succeeded. A read-only
// bind is two calls and the propagation change is a third, so a failure
// after the first one leaves a mount standing that the caller has to
// unwind. Returning only an error would let a caller report a clean
// machine while something is still mounted -- which is the one thing
// camp's failure handling may never do.
func Mount(m plan.Mount) (bool, error) {
	switch m.Kind {
	case plan.Overlay:
		target, err := unix.Open(m.Target, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return false, fmt.Errorf("opening the mount point %s: %w", m.Target, err)
		}
		defer unix.Close(target)
		ends, err := OpenOperands(m)
		if err != nil {
			return false, err
		}
		defer ends.Close()
		if err := Overlay(m, ends, target); err != nil {
			return false, err
		}
	case plan.BindRO, plan.BindRW:
		if err := unix.Mount(m.Source, m.Target, "", unix.MS_BIND, ""); err != nil {
			return false, fmt.Errorf("binding %s onto %s: %w", m.Source, m.Target, err)
		}
	default:
		return false, fmt.Errorf("unknown mount kind %q for %s", m.Kind, m.Target)
	}

	// Everything from here acts on a mount that exists.
	if m.Kind == plan.BindRO {
		if err := remountReadOnly(m.Target); err != nil {
			return true, err
		}
	}

	// Mounts propagate by default on a systemd machine. Without this every
	// mount made inside the composed tree travels back to the backing
	// store's own path, which once turned eight planned mounts into
	// twelve, four of them on the workspace's path.
	if err := unix.Mount("", m.Target, "", unix.MS_PRIVATE, ""); err != nil {
		return true, fmt.Errorf("detaching %s from mount propagation: %w", m.Target, err)
	}
	return true, nil
}

// remountReadOnly turns a fresh bind read-only, replicating whatever the
// kernel has locked on it.
func remountReadOnly(target string) error {
	locked, err := LockedFlagsAt(target)
	if err != nil {
		return err
	}
	flags := uintptr(unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY) | locked
	if err := unix.Mount("", target, "", flags, ""); err != nil {
		return fmt.Errorf("making %s read-only (with the locked flags %s "+
			"replicated): %w", target, DescribeFlags(locked), err)
	}
	return nil
}

// RemountReadOnlyWithoutLockedFlags is the deliberately wrong version,
// and it exists so that a test can assert the fix rather than the bug.
//
// A remount that drops the source's locked flags fails on any nosuid
// filesystem, and this is the call that does it. A test that only showed the correct
// version working could not tell a real fix from a machine where the
// flags happen to be empty; this one reproduces the EPERM on demand.
func RemountReadOnlyWithoutLockedFlags(target string) error {
	flags := uintptr(unix.MS_REMOUNT | unix.MS_BIND | unix.MS_RDONLY)
	if err := unix.Mount("", target, "", flags, ""); err != nil {
		return fmt.Errorf("making %s read-only without replicating its locked "+
			"flags: %w", target, err)
	}
	return nil
}

// lockedNames are the per-mount flags a user namespace will not let a
// remount drop.
var lockedNames = []struct {
	name string
	flag uintptr
}{
	{"nosuid", unix.MS_NOSUID},
	{"nodev", unix.MS_NODEV},
	{"noexec", unix.MS_NOEXEC},
	{"noatime", unix.MS_NOATIME},
	{"nodiratime", unix.MS_NODIRATIME},
	{"relatime", unix.MS_RELATIME},
}

// LockedFlags returns the flags a mount carries that a read-only remount
// has to carry too.
func LockedFlags(entry mountinfo.Entry) uintptr {
	var flags uintptr
	for _, locked := range lockedNames {
		if entry.Has(locked.name) {
			flags |= locked.flag
		}
	}
	// With no atime flag at all the mount is strictatime, and saying so
	// explicitly is what keeps the remount from being read as a request to
	// change it.
	if flags&(unix.MS_NOATIME|unix.MS_RELATIME) == 0 {
		flags |= unix.MS_STRICTATIME
	}
	return flags
}

// LockedFlagsAt reads them from the mount currently at a path.
func LockedFlagsAt(target string) (uintptr, error) {
	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil {
		return 0, err
	}
	entry, found := mountinfo.Top(table, target)
	if !found {
		// Nothing is mounted at the path itself, so the flags that apply to
		// it are the ones of the filesystem it sits on -- which is the
		// question being asked whenever this is called about a directory
		// before anything is bound over it.
		entry, found = mountinfo.Containing(table, target)
		if !found {
			return 0, fmt.Errorf("no mount found at or above %s", target)
		}
	}
	return LockedFlags(entry), nil
}

// DescribeFlags renders a flag set for a message.
func DescribeFlags(flags uintptr) string {
	var named []string
	for _, locked := range lockedNames {
		if flags&locked.flag != 0 {
			named = append(named, locked.name)
		}
	}
	if flags&unix.MS_STRICTATIME != 0 {
		named = append(named, "strictatime")
	}
	if len(named) == 0 {
		return "none"
	}
	return strings.Join(named, ",")
}

// Options renders the described calls as the option string a person
// reads, in the syntax mount(8) takes.
//
// A rendering of the description and never a second derivation from the
// plan, which is what this used to be:
// "lowerdir=a:b,upperdir=c,workdir=d,userxattr" is one legible sentence
// about a composed tree, and it comes from the same calls the comparison
// uses, so the line cannot describe a mount the calls did not make.
func (o OverlayConfig) Options() string {
	var parts []string
	joined := map[string]int{}
	for _, step := range o.Steps {
		if step.Flag() {
			parts = append(parts, step.Key)
			continue
		}
		// One key, however many calls carry it: the lower layers arrive one
		// per call and are read as one colon-separated list.
		key := strings.TrimSuffix(step.Key, "+")
		if at, seen := joined[key]; seen {
			parts[at] += ":" + Escape(step.Path)
			continue
		}
		joined[key] = len(parts)
		parts = append(parts, key+"="+Escape(step.Path))
	}
	return strings.Join(parts, ",")
}

// Mismatch is one thing the mounted filesystem says about itself that
// the description does not.
type Mismatch struct {
	// Key is the operand or the flag that disagrees.
	Key string
	// Want is what the description says and Got what the kernel reports.
	// Both are empty for a flag, which is present or is not.
	Want string
	Got  string
	// Flag says which of those two it is, so a caller can say the right
	// sentence about it.
	Flag bool
}

// Mismatches compares a described overlay with the mount the kernel
// reports at a place.
//
// The one comparison, for the same reason there is one description: the
// verification runs it at a start and 'camp status' runs it on demand,
// and two comparisons could disagree about what "the same mount" means.
//
// Per operand and never as one string: the kernel echoes what was passed
// plus its own defaults -- redirect_dir, uuid, and userxattr inside a
// user namespace whether or not it was asked for -- so string equality
// would fail on a correct mount every time.
func (o OverlayConfig) Mismatches(entry mountinfo.Entry) []Mismatch {
	var found []Mismatch
	var keys []string
	want := map[string]string{}
	for _, step := range o.Steps {
		if step.Flag() {
			if _, present := entry.Super[step.Key]; !present {
				found = append(found, Mismatch{Key: step.Key, Flag: true})
			}
			continue
		}
		key := strings.TrimSuffix(step.Key, "+")
		if _, seen := want[key]; !seen {
			keys = append(keys, key)
			want[key] = step.Path
			continue
		}
		// The layer order is part of what is being compared: the kernel
		// reports the lowers in the order it was given them, and a
		// composition whose lowers were swapped shows the same set in
		// another order.
		want[key] += ":" + step.Path
	}
	for _, key := range keys {
		got := mountinfo.UnescapeOption(option(entry, key))
		if got != want[key] {
			found = append(found, Mismatch{Key: key, Want: want[key], Got: got})
		}
	}
	return found
}

// option reads one of the overlay's operands out of the kernel's table.
//
// Two spellings, one operand. A layer given to the mount API as a
// descriptor is reported under the key that appends it -- "lowerdir+" --
// while the old option-string form reports "lowerdir". camp mounts the
// composed tree the first way and reads both, because a mount made by an
// older camp, or by hand, is still a mount this comparison may be asked
// about.
func option(entry mountinfo.Entry, key string) string {
	if value, found := entry.Super[key]; found {
		return value
	}
	return entry.Super[key+"+"]
}

// Escape protects a path used inside a mount option.
//
// ":" separates lower directories and "," separates options, so a path
// containing either would otherwise be read as structure. The backslash
// escapes both and has to be escaped first.
func Escape(path string) string {
	return strings.NewReplacer(`\`, `\\`, `:`, `\:`, `,`, `\,`).Replace(path)
}

// Unmount removes one mount point, and never lazily.
//
// The path is resolved by the kernel, and that is acceptable here for one
// reason: the only caller is a rollback inside the session's own private
// mount namespace, where nothing but this process has been able to mount
// anything, and where whatever is not removed goes with the namespace.
func Unmount(target string) (Outcome, error) {
	return outcome(unix.Unmount(target, 0), target)
}

// outcome reads what umount2 answered, and names the mount the way a
// person would.
func outcome(err error, named string) (Outcome, error) {
	switch {
	case err == nil:
		return Unmounted, nil
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOENT):
		// EINVAL from umount(2) means "not a mount point", which is the job
		// already done however it came about.
		return Absent, nil
	case errors.Is(err, unix.EBUSY):
		return Busy, fmt.Errorf("%s is still in use", named)
	default:
		return Busy, fmt.Errorf("unmounting %s: %w", named, err)
	}
}
