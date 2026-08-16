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
// MNT_DETACH appears nowhere in this package and nowhere in camp. A lazy
// unmount removes a mount from the table while it is still alive and
// still being written through -- measured -- and in the privileged mode
// the table is the only guard against a second composition on the same
// upper. It was --force wearing another name.
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

// Mount performs one operation and leaves it private.
func Mount(m plan.Mount) error {
	switch m.Kind {
	case plan.Overlay:
		if err := unix.Mount("overlay", m.Target, "overlay", 0, Options(m)); err != nil {
			return fmt.Errorf("mounting the overlay at %s: %w", m.Target, err)
		}
	case plan.BindRO, plan.BindRW:
		if err := unix.Mount(m.Source, m.Target, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("binding %s onto %s: %w", m.Source, m.Target, err)
		}
		if m.Kind == plan.BindRO {
			if err := remountReadOnly(m.Target); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown mount kind %q for %s", m.Kind, m.Target)
	}

	// Mounts propagate by default on a systemd machine. Without this every
	// mount made inside the composed tree travels back to the backing
	// store's own path, which once turned eight planned mounts into
	// twelve, four of them on the workspace's path.
	if err := unix.Mount("", m.Target, "", unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("detaching %s from mount propagation: %w", m.Target, err)
	}
	return nil
}

// MountByDescriptor performs one operation with both ends already opened,
// so that the object checked is the object mounted.
//
// Used by the privileged helper. Between the front end validating a path
// and the helper mounting it, a component the user owns can be replaced
// with a symlink -- the front end's check would then be about a different
// object than the mount. Referring to the endpoints through
// /proc/self/fd/N closes that: a descriptor names the object it was
// opened on, whatever happens to the name afterwards.
func MountByDescriptor(m plan.Mount, sourceFD, targetFD int) error {
	target := fmt.Sprintf("/proc/self/fd/%d", targetFD)
	switch m.Kind {
	case plan.Overlay:
		if err := unix.Mount("overlay", target, "overlay", 0, Options(m)); err != nil {
			return fmt.Errorf("mounting the overlay at %s: %w", m.Target, err)
		}
	case plan.BindRO, plan.BindRW:
		source := fmt.Sprintf("/proc/self/fd/%d", sourceFD)
		if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("binding %s onto %s: %w", m.Source, m.Target, err)
		}
		if m.Kind == plan.BindRO {
			if err := remountReadOnly(target); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown mount kind %q for %s", m.Kind, m.Target)
	}
	if err := unix.Mount("", target, "", unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("detaching %s from mount propagation: %w", m.Target, err)
	}
	return nil
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
		// The path may be reached through a descriptor, in which case the
		// mount point is not the string we have. Fall back to the mount the
		// path sits on, which carries the same locked flags.
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

// Options renders the -o string for an overlay.
func Options(m plan.Mount) string {
	lowers := make([]string, 0, len(m.Lower))
	for _, dir := range m.Lower {
		lowers = append(lowers, Escape(dir))
	}
	parts := []string{"lowerdir=" + strings.Join(lowers, ":")}
	if m.Upper != "" {
		parts = append(parts, "upperdir="+Escape(m.Upper), "workdir="+Escape(m.Work))
	}
	if m.Xattr != "" {
		parts = append(parts, m.Xattr)
	}
	return strings.Join(parts, ",")
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
func Unmount(target string) (Outcome, error) {
	err := unix.Unmount(target, 0)
	switch {
	case err == nil:
		return Unmounted, nil
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOENT):
		// EINVAL from umount(2) means "not a mount point", which is the job
		// already done however it came about.
		return Absent, nil
	case errors.Is(err, unix.EBUSY):
		return Busy, fmt.Errorf("%s is still in use", target)
	default:
		return Busy, fmt.Errorf("unmounting %s: %w", target, err)
	}
}

// Move relocates a whole mount tree onto another directory.
//
// The privileged mode builds the composition in a staging directory and
// moves it onto the live path only once it has been verified, so nothing
// outside ever sees a half-built tree. Submounts follow the move, which
// is why the staging parent has to be private.
func Move(from, to string) error {
	if err := unix.Mount(from, to, "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("moving the verified tree from %s onto %s: %w", from, to, err)
	}
	return nil
}
