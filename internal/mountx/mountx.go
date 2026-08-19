// Package mountx performs a plan, and takes it apart again.
//
// Deciding what to mount and mounting it are separate responsibilities.
// The plan package decides; this one acts, and it acts by syscall in both
// modes -- in the namespace mode from inside the namespace camp created,
// and in the privileged mode from inside the narrow helper that sudo
// wraps. There is no shelling out to mount(8) anywhere: the messages it
// prints are translated, its exit codes are coarse, and the plan is
// already a data structure that does not need re-serialising into
// command-line syntax to be executed.
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
// filesystem. The flags are
// read back from the mount that the bind just created, so what is
// replicated is what the kernel actually has rather than what anybody
// assumed.
//
// What the kernel is told about the composed tree is derived once, in
// one object, and the mount is performed from it. The state record keeps
// that object, the verification and 'camp status' compare a mounted
// overlay against it, and 'camp plan' prints it. What that replaced was
// four renderings of the same operands -- the fsconfig calls, a legacy
// option string built for the record, an expectation composed inside the
// verification, and the printed call sequence -- none of which was ever
// compared with any other, so a change to one of them was a record, a
// check or a plan describing a mount nobody made.
//
// MNT_DETACH appears nowhere in this package and nowhere in camp. A lazy
// unmount removes a mount from the table while it is still alive and
// still being written through -- measured -- and in the privileged mode
// the table is the only guard against a second composition on the same
// upper. It was --force wearing another name.
//
// The privileged helper's binds are made with open_tree and move_mount,
// and the attribute changes that follow them are addressed through the
// created mount's own /proc/self/fd name. That sequence was written
// against the documented contract of those calls, because nothing in
// this repository may mount.
//
// It has since been run (measured, kernel 7.0.0-29, through the
// namespace mode's own end-to-end test): the clone is attached where the
// target descriptor points, the target shows the source, the read-only
// remount through the clone's descriptor takes, and so does the
// propagation change. The control in the same test is what makes that a
// measurement rather than a coincidence -- the identical MS_PRIVATE call
// through the descriptor opened *before* the mount still fails, so it is
// the clone's descriptor that made the difference. What is still unrun
// is the privileged mode's own choreography around these calls, not the
// calls.
//
// One narrow thing was measured before any of that, and it is why the
// shape was trusted enough to land: open_tree(fd, "",
// OPEN_TREE_CLONE|AT_EMPTY_PATH) on an O_PATH descriptor of a directory
// answers EPERM to a process without CAP_SYS_ADMIN, and not ENOSYS or
// EINVAL (measured, kernel 7.0.0-29). The kernel validates the flags and
// resolves the empty path against the descriptor before it asks about
// the capability.
//
// And camp does not believe any of these calls in any case. Verification
// runs after the mounts and inspects the mount table and the tree
// itself, in staging and again at the live path, so a flag that did not
// take or a mount that landed somewhere else comes back as a refusal and
// a rollback -- the same way the read-only bind that MS_BIND silently
// ignores has always been caught.
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
// It exists because three things have to say the same thing about one
// mount and used to say it three times. The helper performed one
// fsconfig per operand; the state record rebuilt a legacy option string
// from the plan's pathnames; the verification composed a third
// expectation out of the same fields; and nothing compared any two of
// them. An operand added, reordered or renamed in one of the three was a
// record describing a mount that was made differently, with every test
// still green.
//
// So this is the one derivation, and the mount is performed *from* it.
// Changing what the kernel is sent is changing this object, which the
// record persists and the verification and 'camp status' compare against
// the mounted filesystem. There is no side left to drift from.
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
	// a flag; a record keeps the two fields above and not these, because
	// what a comparison needs is what the kernel was told.
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
// the syscalls, the record, the verification, the printed plan -- reads
// it from here, so an operand added or reordered here is added or
// reordered in all of them at once.
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
// For the caller that resolved them itself and is mounting in its own
// namespace: there is no privilege boundary between the check and the
// mount there, and the paths came out of a plan that refuses a repository
// which is a symlink. The privileged helper does not use this -- it opens
// each operand beneath the base it was given, following nothing, and
// compares what it opened against the identity the front end recorded.
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
//
// It is split out only because MountByDescriptor has to keep that
// descriptor. A mount's own handle is the one name for it that no later
// resolution can redirect, and the privileged path addresses the attached
// mount through it -- so there the attach cannot be the last thing that
// happens, the way it is above.
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
// list, which is the same list the record keeps and the verification
// compares against the mounted filesystem. It is a function of its own
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

// MountByDescriptor performs one operation with both ends already opened,
// so that the object checked is the object mounted.
//
// Used by the privileged helper. Between the front end validating a path
// and the helper mounting it, a component the user owns can be replaced
// with a symlink -- the front end's check would then be about a different
// object than the mount. Referring to the endpoints through descriptors
// closes that: a descriptor names the object it was opened on, whatever
// happens to the name afterwards.
//
// A bind here is a detached copy that is then attached, and not a
// mount(2) between two /proc/self/fd names, because of what each leaves
// behind. mount(2) leaves nothing that names the mount it just made: an
// O_PATH descriptor holds the mount and dentry it was opened on, and
// stacking a mount on that dentry does not change it, so the descriptor
// that placed the bind still refers to the object underneath it. The
// read-only remount and the propagation change therefore had to be made
// through the target opened a second time, by name -- and a fresh
// resolution of a name cannot prove it found the mount just made. That
// was the hole. open_tree with OPEN_TREE_CLONE returns a descriptor on a
// new mount, move_mount attaches that mount to the target descriptor, and
// the descriptor goes on naming the mount and not the name it landed on.
// Everything after the attach is addressed through it.
//
// The clone is not recursive. OPEN_TREE_CLONE on its own copies one
// mount, which is what MS_BIND makes; AT_RECURSIVE is what --rbind does,
// and camp does not do that.
//
// Deliberately not mount_setattr, which is where somebody reading this
// will reach next. It would set the read-only attribute on the clone
// while it is still detached, so there would be no moment at which the
// bind is attached and writable -- strictly better than remounting it
// afterwards. But mount_setattr is Linux 5.12 and open_tree, move_mount
// and fsopen are 5.2, and camp already hard-requires fsopen for the
// composed tree. Raising the kernel floor is a decision that needs a
// measurement behind it and there is none here. Making the remount camp
// already makes through the clone's descriptor keeps the syscall set
// where it is and still closes the reopen, which is what this is about.
//
// It reports whether a mount now exists at the target, for the reason
// Mount gives: the value becomes true the moment move_mount succeeds,
// because a failure in the remount or the propagation change after that
// leaves a mount standing that the caller has to unwind.
func MountByDescriptor(m plan.Mount, sourceFD, targetFD int, ends Operands) (bool, error) {
	var mounted int
	var err error
	// What the message says when the attach itself fails, which is the one
	// step whose failure means nothing was mounted.
	var attaching string

	switch m.Kind {
	case plan.Overlay:
		mounted, err = overlayMount(m, ends)
		attaching = fmt.Sprintf("attaching the overlay to %s", m.Target)
	case plan.BindRO, plan.BindRW:
		// A detached copy of the mount the source descriptor is on, taken
		// through that descriptor and not through any name.
		mounted, err = unix.OpenTree(sourceFD, "",
			unix.OPEN_TREE_CLONE|unix.AT_EMPTY_PATH|unix.OPEN_TREE_CLOEXEC)
		if err != nil {
			err = fmt.Errorf("taking a detached copy of %s to bind onto %s: %w",
				m.Source, m.Target, err)
		}
		attaching = fmt.Sprintf("binding %s onto %s", m.Source, m.Target)
	default:
		return false, fmt.Errorf("unknown mount kind %q for %s", m.Kind, m.Target)
	}
	if err != nil {
		return false, err
	}
	defer unix.Close(mounted)

	if err := unix.MoveMount(mounted, "", targetFD, "",
		unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); err != nil {
		return false, fmt.Errorf("%s: %w", attaching, err)
	}

	// The mount exists now, and this descriptor is what names it. Before
	// the move its /proc/self/fd entry named a mount attached to nothing;
	// after the move it names the mount at the target.
	handle := fmt.Sprintf("/proc/self/fd/%d", mounted)

	if m.Kind == plan.BindRO {
		locked, err := lockedFlagsOf(mounted)
		if err != nil {
			return true, fmt.Errorf("reading the flags the kernel locked on %s: %w",
				m.Target, err)
		}
		if err := remount(handle, m.Target, locked); err != nil {
			return true, err
		}
	}
	if err := unix.Mount("", handle, "", unix.MS_PRIVATE, ""); err != nil {
		return true, fmt.Errorf("detaching %s from mount propagation: %w", m.Target, err)
	}
	return true, nil
}

// stFlags maps what statfs reports to what a remount has to ask for.
var stFlags = []struct {
	reported int64
	remount  uintptr
}{
	{unix.ST_NOSUID, unix.MS_NOSUID},
	{unix.ST_NODEV, unix.MS_NODEV},
	{unix.ST_NOEXEC, unix.MS_NOEXEC},
	{unix.ST_NOATIME, unix.MS_NOATIME},
	{unix.ST_NODIRATIME, unix.MS_NODIRATIME},
	{unix.ST_RELATIME, unix.MS_RELATIME},
}

// lockedFlagsOf reads the locked flags from the mount a descriptor names.
//
// Asked of the kernel through the descriptor rather than looked up in
// mountinfo by path, for two reasons. A descriptor-held mount has no name
// to look up -- its /proc/self/fd path is not a mount point, and a
// lookup by that string silently falls through to the flags of /proc
// itself, which are not the source's. And a path lookup would reopen the
// window the descriptor exists to close. Measured: statfs and mountinfo
// agree on exactly this flag set, on ext4, tmpfs and procfs alike.
func lockedFlagsOf(fd int) (uintptr, error) {
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return 0, err
	}
	var flags uintptr
	for _, pair := range stFlags {
		if st.Flags&pair.reported != 0 {
			flags |= pair.remount
		}
	}
	// With no atime flag at all the mount is strictatime, and saying so
	// explicitly is what keeps the remount from being read as a request to
	// change it.
	if flags&(unix.MS_NOATIME|unix.MS_RELATIME) == 0 {
		flags |= unix.MS_STRICTATIME
	}
	return flags, nil
}

// remountReadOnly turns a fresh bind read-only, replicating whatever the
// kernel has locked on it.
func remountReadOnly(target string) error {
	locked, err := LockedFlagsAt(target)
	if err != nil {
		return err
	}
	return remount(target, target, locked)
}

// remount is the second of the two calls a read-only bind takes.
//
// handle is what the kernel is addressed through -- a path, or a
// descriptor's /proc/self/fd name -- and named is what a person should
// read in the message, which are not the same string when the mount is
// held by descriptor.
func remount(handle, named string, locked uintptr) error {
	flags := uintptr(unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY) | locked
	if err := unix.Mount("", handle, "", flags, ""); err != nil {
		return fmt.Errorf("making %s read-only (with the locked flags %s "+
			"replicated): %w", named, DescribeFlags(locked), err)
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
//
// Only ever a real path. A mount held by descriptor has none -- its
// /proc/self/fd name is not a mount point, and asking here for one would
// fall through to the flags of /proc itself, which are not the source's.
// That is what the descriptor path used to do; it reads them from the
// descriptor now.
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
// plan, which is what this used to be: the record keeps this line
// because "lowerdir=a:b,upperdir=c,workdir=d,userxattr" is one legible
// sentence about a composed tree, and it keeps the calls beside it
// because that is what a comparison needs. Both come from here, so the
// line cannot describe a mount the calls did not make.
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
// verification runs it at up against the plan, and 'camp status' runs it
// against the record, and two comparisons could disagree about what
// "the same mount" means.
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
// The path is resolved by the kernel, so this is only for a name nobody
// but root can change any part of -- inside the graveyard, or the
// namespace mode's own private table. Anything under an environment goes
// through UnmountIn, and the comment there says what happened when it did
// not.
func Unmount(target string) (Outcome, error) {
	return outcome(unix.Unmount(target, 0), target)
}

// UnmountIn removes whatever is mounted at one name inside a directory
// the caller holds open.
//
// umount2 takes a path and nothing else, so a descriptor cannot be handed
// to it directly. What can be handed to it is the descriptor's own
// /proc/self/fd name with the one component appended: the kernel resolves
// the magic link to the directory that descriptor holds -- not by walking
// the name it was opened by -- and then walks exactly one component from
// there. So no rename anywhere above it can point this somewhere else,
// and C34 says the one component below it cannot be renamed either while
// something is mounted on it.
//
// This is not a refinement. Measured by the rename race, at base-owned:
// with the environment's name swapped for a link to a root-owned tree,
// root unmounted a mount in *that* tree, because the rollback removed
// camp's self-bind by the name it had written down. A mount that cannot
// be moved has no graveyard route, and the name route reached wherever
// the link pointed.
//
// named is what a person reads in a message, and never what the kernel is
// given. A caller with no directory descriptor gets the plain name and the
// window that comes with it.
func UnmountIn(dir int, name, named string) (Outcome, error) {
	if dir < 0 || name == "" {
		return Unmount(named)
	}
	return outcome(unix.Unmount(fmt.Sprintf("/proc/self/fd/%d/%s", dir, name), 0), named)
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

// Move relocates a whole mount tree onto another directory.
//
// The privileged mode builds the composition in a staging directory and
// moves it onto the live path only once it has been verified, so nothing
// outside ever sees a half-built tree. Submounts follow the move, which
// is why the staging parent has to be private.
func Move(fromFD, toFD int, from, to string) error {
	// By descriptor, not by name. This is the last step of the privileged
	// mode and the one that makes the composition visible to the machine:
	// naming the two ends here would resolve them again, at the one moment
	// when a swapped live directory would be root attaching a verified tree
	// somewhere nobody asked for.
	if err := unix.MoveMount(fromFD, "", toFD, "",
		unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); err != nil {
		return fmt.Errorf("moving the verified tree from %s onto %s: %w", from, to, err)
	}
	return nil
}

// Detach makes one mount point its own private mount, by descriptor.
//
// It is what the staging tree needs before anything is built in it. The
// kernel refuses MS_MOVE with EINVAL when the mount being moved has a
// shared parent, and on a systemd machine / is shared:1 -- so a
// composition built straight into a directory on / cannot be moved
// anywhere, which is the whole staging design. Binding the directory onto
// itself and making that bind private gives everything mounted inside it
// a private parent, without touching the propagation of / itself: this
// mode already has exactly two machine-wide effects, and a third one --
// changing how mounts propagate for every process -- is not available to
// it.
//
// The namespace mode never needed this because it makes its whole
// namespace private on entry, which is also why nothing caught it.
//
// Both halves are addressed by descriptor, which is the whole difference
// from what this used to be. The self-bind is a detached clone taken
// through fd and moved back onto fd, and the propagation change goes
// through the clone's own descriptor -- so neither call resolves the
// directory's name, and neither can be pointed at something else by
// renaming it. named is only what a person reads in a message.
// MountByDescriptor's comment carries the argument for the primitives and
// for why mount_setattr is not among them.
//
// It reports whether the self-bind now exists, the way Mount and
// MountByDescriptor do, and for the same reason: the bind and the
// propagation change are two calls, and a caller that recorded this only
// when the whole thing succeeded would unwind a machine it believes is
// clean while camp's own mount is still standing.
func Detach(fd int, named string) (bool, error) {
	clone, err := unix.OpenTree(fd, "",
		unix.OPEN_TREE_CLONE|unix.AT_EMPTY_PATH|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return false, fmt.Errorf("taking a detached copy of %s to bind onto "+
			"itself so that what is built in it can be moved: %w", named, err)
	}
	defer unix.Close(clone)

	if err := unix.MoveMount(clone, "", fd, "",
		unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); err != nil {
		return false, fmt.Errorf("binding %s onto itself so that what is built "+
			"in it can be moved: %w", named, err)
	}

	if err := unix.Mount("", fmt.Sprintf("/proc/self/fd/%d", clone), "",
		unix.MS_PRIVATE, ""); err != nil {
		// The bind stands and the caller is told so, which is the whole point
		// of the flag: the self-bind has to come off and this does not take it
		// off itself. Removing camp's own mounts is one operation with one
		// route through the graveyard, and it belongs to the caller that keeps
		// the rollback list -- a second removal path here would be the one
		// unmount in the privileged half that nothing else can see or report.
		return true, fmt.Errorf("making %s private so that what is built in it "+
			"can be moved: %w", named, err)
	}
	return true, nil
}
