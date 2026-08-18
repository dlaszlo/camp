// Package fsx is the only place in camp that writes to a filesystem.
//
// That is not a tidiness preference. The first invariant -- camp only
// composes, it never modifies a repository -- is a property of the source
// code, and the way to make it checkable is to have one door and to be
// able to say what is behind it. Every create, write, chmod and remove
// camp performs goes through this package, and every one of them is
// addressed relative to an Area: one of the places camp owns.
//
//	work     $ENV/.camp/work/<hash>      disposable, swept when nothing is mounted
//	storage  $ENV/.camp/storage/<hash>   persistent, never removed by camp
//	state    $XDG_STATE_HOME/camp        the privileged mode's records
//	reports  $ENV/.camp/reports          what a namespace session leaves behind
//	logs     $ENV/.camp/logs             every line camp wrote to stderr
//
// **An area is a base and the components below it, and every one of those
// components is resolved by the kernel, in the call that acts on it,
// following no symlink and never leaving the base.** That is what makes
// the invariant true rather than merely intended. Joining strings and
// calling MkdirAll would follow whatever a symlink at $ENV/.camp/work
// pointed at -- into a repository, if somebody put it there -- and no
// check made beforehand could help, because the check and the write would
// be two resolutions of the same name with a gap between them. There is
// no gap here: openat2 resolves and acts at once, and refuses a symlink
// and an escape itself.
//
// What camp trusts is the base. The environment root is resolved once,
// when the configuration is read, and it is the only path camp ever
// follows symlinks through; everything below it is camp's own and is
// resolved the strict way.
//
// The one thing camp deletes outside these areas is the kernel's own
// leftover work directory inside its work area, which is inside work
// anyway.
package fsx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/pathx"
)

// Area is a directory camp owns, and the root of everything it may write.
type Area struct {
	// Kind names the area for a message: "work", "storage", "state",
	// "reports", "logs".
	Kind string
	// base is the directory camp trusts, and parts are the components
	// below it that make up this area. They are kept apart because the
	// base is the point resolution starts from and never leaves.
	base  string
	parts []string
}

// ErrOutside is returned when a path would leave its area. It is a
// programming error, never a user's, and it fails loudly rather than
// writing somewhere plausible.
var ErrOutside = errors.New("the path leaves the area camp may write in")

// At builds an area from a base camp trusts and the components below it.
//
// The base is where the confinement starts, so a caller passes the
// highest directory it has already established is not a repository and
// not reached through anybody's symlink -- the environment root, for
// everything under .camp.
func At(kind, base string, parts ...string) Area {
	return Area{Kind: kind, base: base, parts: append([]string(nil), parts...)}
}

// Work is the disposable area for one composition.
func Work(env, hash string) Area { return At("work", env, config.Dir, "work", hash) }

// Storage is the persistent area for one composition. camp never removes
// it: it holds unfinished worktrees and machine-local state.
func Storage(env, hash string) Area { return At("storage", env, config.Dir, "storage", hash) }

// State is where the privileged mode's records live. Its base is the
// user's own state directory, which is not camp's to vouch for; camp's
// own directory below it is.
func State(base, name string) Area { return At("state", base, name) }

// Reports is where a namespace session leaves its end-of-session report.
func Reports(env string) Area { return At("reports", env, config.Dir, "reports") }

// Logs is where camp keeps what it said. It is the one area written by
// appending rather than by replacing: a log is a record of a sequence,
// and rewriting the file to add a line to it would lose the sequence at
// every crash.
func Logs(env string) Area { return At("logs", env, config.Dir, "logs") }

// Live is the composed tree's own directory, and the only Area that is
// not somewhere camp keeps files: nothing is ever written inside it --
// mounts are made onto it -- and the single operation it exists for is
// creating the empty directory itself, which git cannot record and no
// clone can therefore bring.
//
// It does not weaken what an Area is for. The composed tree can never be
// inside a repository: the validation refuses that outright, and the one
// caller checks the same thing again before it creates anything, because
// it runs before the validation does.
func Live(env string, parts ...string) Area { return At("live", env, parts...) }

// Scratch makes a directory that belongs to nobody else and returns it as
// an area, with the call that removes it again.
//
// For the capability probe: it builds a real overlay somewhere harmless
// to find out whether this machine can, inside a namespace that vanishes
// with the process. It is still camp writing to a filesystem, so it goes
// through the one door like everything else -- and because the door is
// the only place that writes, the probe's tree cannot be anywhere near a
// repository either.
func Scratch(prefix string) (Area, func(), error) {
	root, err := os.MkdirTemp("", prefix)
	if err != nil {
		return Area{}, func() {}, fmt.Errorf("making a scratch directory: %w", err)
	}
	area := Area{Kind: "scratch", base: root}
	return area, func() { _ = removeTree(root) }, nil
}

// removeTree is Scratch's own cleanup: the whole directory, including
// itself.
func removeTree(root string) error {
	parent, err := unix.Open(filepath.Dir(root), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	return removeTreeAt(parent, filepath.Base(root))
}

// Camp is $ENV/.camp itself: the configuration, and the stores below it.
// Only 'camp init' writes here, and only the files a person asked camp to
// create.
func Camp(env string) Area { return At("camp", env, config.Dir) }

// Root returns the area's own directory, as a path.
//
// For messages, and for handing to the kernel as a mount operand. camp's
// own writes never go through it: they are addressed by component from
// the base, which is the whole point of the type.
func (a Area) Root() string {
	return filepath.Join(append([]string{a.base}, a.parts...)...)
}

// Path resolves a relative path inside the area, refusing anything that
// would climb out of it.
//
// The same caveat as Root: what comes back is a name for a message or an
// operand for the kernel, and never the way camp writes.
func (a Area) Path(parts ...string) (string, error) {
	if a.base == "" {
		return "", fmt.Errorf("an empty %s area was used", a.Kind)
	}
	if err := components(parts); err != nil {
		return "", err
	}
	return filepath.Join(append([]string{a.Root()}, parts...)...), nil
}

// Sub returns the area rooted at a subdirectory of this one, so that a
// caller working under work/gen cannot reach the rest of work.
func (a Area) Sub(parts ...string) (Area, error) {
	if err := a.usable(parts); err != nil {
		return Area{}, err
	}
	return Area{Kind: a.Kind, base: a.base, parts: a.below(parts...)}, nil
}

// Ensure creates the area's own directory, and every directory above it
// down from the base, with a mode of its own for the area itself.
//
// The state directory is 0700 and its records are 0600: a record names
// every path of a composition, and that is nobody else's business.
func (a Area) Ensure(mode os.FileMode) error {
	if a.base == "" {
		return fmt.Errorf("an empty %s area was used", a.Kind)
	}
	fd, err := a.make(nil, mode)
	if err != nil {
		return err
	}
	unix.Close(fd)
	return nil
}

// RemoveSelf removes the area's own directory once it is empty.
func (a Area) RemoveSelf() error {
	if len(a.parts) == 0 {
		return fmt.Errorf("the %s area's own base is not camp's to remove", a.Kind)
	}
	return a.unlink(nil, unix.AT_REMOVEDIR)
}

// MkdirAll creates a directory inside the area, and every directory above
// it up to the area's own root.
func (a Area) MkdirAll(parts ...string) (string, error) {
	return a.MkdirAllMode(0o755, parts...)
}

// MkdirAllMode creates a directory with a mode of its own -- the
// privileged mode's staging root is 0700, because until the move it is
// the only place the half-built composition exists.
func (a Area) MkdirAllMode(mode os.FileMode, parts ...string) (string, error) {
	if err := a.usable(parts); err != nil {
		return "", err
	}
	fd, err := a.make(parts, mode)
	if err != nil {
		return "", err
	}
	unix.Close(fd)
	return a.Path(parts...)
}

// MkdirDeep creates a directory named by a whole relative path, one
// component at a time, so a mirrored target path can be reproduced inside
// storage.
func (a Area) MkdirDeep(components []string) (string, error) {
	if len(components) == 0 {
		return a.Root(), nil
	}
	return a.MkdirAll(components...)
}

// Touch creates an empty regular file if it is not there, and reports
// whether it had to create it.
//
// This is how an attachment point is made for a file island: the
// placeholder lives in camp's own storage, never in a repository.
func (a Area) Touch(parts ...string) (string, bool, error) {
	if err := a.usable(parts); err != nil {
		return "", false, err
	}
	dir, name, err := a.parent(parts)
	if err != nil {
		return "", false, err
	}
	defer unix.Close(dir)

	var st unix.Stat_t
	if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		path, _ := a.Path(parts...)
		return path, false, nil
	}
	fd, err := openAt(dir, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY, 0o644)
	if err != nil {
		return "", false, fmt.Errorf("creating %s in %s: %w", name, a.Kind, err)
	}
	unix.Close(fd)
	path, _ := a.Path(parts...)
	return path, true, nil
}

// Append opens a file inside the area for appending, creating it if it is
// not there.
//
// O_APPEND, so that a whole line written in one call cannot interleave
// with another process's line: camp's launcher and a session's init write
// to one log, and the kernel places an appending write at the end of the
// file as one operation.
func (a Area) Append(name string, mode os.FileMode) (*os.File, error) {
	if err := a.usable([]string{name}); err != nil {
		return nil, err
	}
	dir, last, err := a.parent([]string{name})
	if err != nil {
		return nil, err
	}
	defer unix.Close(dir)

	fd, err := openAt(dir, last, unix.O_APPEND|unix.O_CREAT|unix.O_WRONLY, mode)
	if err != nil {
		return nil, fmt.Errorf("opening %s in %s: %w", name, a.Kind, err)
	}
	path, _ := a.Path(name)
	return os.NewFile(uintptr(fd), path), nil
}

// Rename moves one name to another inside the area, which is how a log is
// rotated: both ends are resolved beneath the area, so nothing can be
// renamed out of it or over something outside it.
func (a Area) Rename(from, to string) error {
	if err := a.usable([]string{from, to}); err != nil {
		return err
	}
	fromDir, fromName, err := a.parent([]string{from})
	if err != nil {
		return err
	}
	defer unix.Close(fromDir)
	toDir, toName, err := a.parent([]string{to})
	if err != nil {
		return err
	}
	defer unix.Close(toDir)

	if err := unix.Renameat(fromDir, fromName, toDir, toName); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("renaming %s to %s in %s: %w", from, to, a.Kind, err)
	}
	return nil
}

// Write replaces a file inside the area, atomically.
//
// Written to a temporary file in the same directory and renamed, with
// both file and directory synced, because a record that is half written
// still parses and describes half a composition.
func (a Area) Write(name string, data []byte, mode os.FileMode) error {
	if err := a.usable([]string{name}); err != nil {
		return err
	}
	dir, last, err := a.parent([]string{name})
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	return writeAt(dir, last, data, mode, a.Kind)
}

// writeAt is the atomic replacement, performed entirely through a
// directory descriptor so that no part of it re-resolves a path.
func writeAt(dir int, name string, data []byte, mode os.FileMode, kind string) error {
	temporary := "." + name + ".camp"
	fd, err := openAt(dir, temporary, unix.O_CREAT|unix.O_TRUNC|unix.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("writing %s in %s: %w", name, kind, err)
	}
	file := os.NewFile(uintptr(fd), temporary)
	defer func() {
		file.Close()
		unix.Unlinkat(dir, temporary, 0)
	}()

	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("writing %s in %s: %w", name, kind, err)
	}
	if err := file.Chmod(mode); err != nil {
		return fmt.Errorf("setting the mode of %s in %s: %w", name, kind, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("syncing %s in %s: %w", name, kind, err)
	}
	if err := unix.Renameat(dir, temporary, dir, name); err != nil {
		return fmt.Errorf("replacing %s in %s: %w", name, kind, err)
	}
	return syncFd(dir)
}

func syncFd(dir int) error {
	// The directory entry itself has to reach the disk, or the rename can
	// be lost while the file it named survives.
	duplicate, err := unix.Openat(dir, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("syncing the directory: %w", err)
	}
	defer unix.Close(duplicate)
	if err := unix.Fsync(duplicate); err != nil {
		return fmt.Errorf("syncing the directory: %w", err)
	}
	return nil
}

// Remove deletes one thing inside the area.
func (a Area) Remove(parts ...string) error {
	if err := a.usable(parts); err != nil {
		return err
	}
	if err := a.unlink(parts, 0); err == nil {
		return nil
	} else if !errors.Is(err, unix.EISDIR) {
		return err
	}
	return a.unlink(parts, unix.AT_REMOVEDIR)
}

// RemoveTree deletes a whole subtree of the area.
//
// The kernel leaves a work/ directory inside the overlay's work
// directory, mode 000 and owned by the invoking user, which cannot be
// walked until it is chmodded -- so this makes every directory it meets
// traversable on the way down. Every step is taken through a descriptor
// of the directory being emptied, so nothing here can be redirected by a
// symlink appearing halfway through the walk.
func (a Area) RemoveTree(parts ...string) error {
	if err := a.usable(parts); err != nil {
		return err
	}
	dir, name, err := a.parent(parts)
	if err != nil {
		if isAbsent(err) {
			return nil
		}
		return err
	}
	defer unix.Close(dir)
	return removeTreeAt(dir, name)
}

func removeTreeAt(dir int, name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if isAbsent(err) {
			return nil
		}
		return fmt.Errorf("looking at %s: %w", name, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Unlinkat(dir, name, 0); err != nil && !isAbsent(err) {
			return fmt.Errorf("removing %s: %w", name, err)
		}
		return nil
	}

	// The kernel's leftover is mode 000; without this it cannot even be
	// listed, let alone emptied.
	if st.Mode&0o300 != 0o300 {
		if err := unix.Fchmodat(dir, name, 0o700, 0); err != nil {
			return fmt.Errorf("making %s removable: %w", name, err)
		}
	}
	child, err := openAt(dir, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		if isAbsent(err) {
			return nil
		}
		return fmt.Errorf("opening %s: %w", name, err)
	}
	names, err := readNames(child)
	if err != nil {
		unix.Close(child)
		return fmt.Errorf("listing %s: %w", name, err)
	}
	for _, entry := range names {
		if err := removeTreeAt(child, entry); err != nil {
			unix.Close(child)
			return err
		}
	}
	unix.Close(child)
	if err := unix.Unlinkat(dir, name, unix.AT_REMOVEDIR); err != nil && !isAbsent(err) {
		return fmt.Errorf("removing %s: %w", name, err)
	}
	return nil
}

// Chown gives everything in the area to a user.
//
// Used by the privileged helper for the one thing the kernel creates as
// root: the overlay's leftover work directory. The path camp guarantees
// writable must not end up owned by root -- and root walking a tree by
// path is exactly where a symlink would be waiting, so this walk is by
// descriptor too.
func (a Area) Chown(uid, gid int, parts ...string) error {
	if err := a.usable(parts); err != nil {
		return err
	}
	dir, name, err := a.parent(parts)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	return chownTreeAt(dir, name, uid, gid)
}

func chownTreeAt(dir int, name string, uid, gid int) error {
	if err := unix.Fchownat(dir, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if isAbsent(err) {
			return nil
		}
		return fmt.Errorf("giving %s to uid %d: %w", name, uid, err)
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if isAbsent(err) {
			return nil
		}
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil
	}
	child, err := openAt(dir, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		if isAbsent(err) {
			return nil
		}
		return err
	}
	defer unix.Close(child)
	names, err := readNames(child)
	if err != nil {
		return err
	}
	for _, entry := range names {
		if err := chownTreeAt(child, entry, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// -- resolution -------------------------------------------------------------

// usable reports whether this area and these components can be addressed
// at all.
func (a Area) usable(parts []string) error {
	if a.base == "" {
		return fmt.Errorf("an empty %s area was used", a.Kind)
	}
	return components(parts)
}

// components refuses anything that is not one plain name. Nothing else
// could climb out of the area, but a check that runs before the syscall
// gives the caller a message about its own mistake rather than an EXDEV.
func components(parts []string) error {
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\x00") {
			return fmt.Errorf("%w: %q is not a single path component", ErrOutside, part)
		}
	}
	return nil
}

// below is this area's own components followed by more, copied so that no
// caller can grow another area's path by appending to a shared array.
func (a Area) below(parts ...string) []string {
	whole := make([]string, 0, len(a.parts)+len(parts))
	whole = append(whole, a.parts...)
	return append(whole, parts...)
}

// parent opens the directory holding the last component, resolved from
// the base downwards, and returns it with that name.
func (a Area) parent(parts []string) (int, string, error) {
	whole := a.below(parts...)
	if len(whole) == 0 {
		return -1, "", fmt.Errorf("the %s area's own base is not camp's to write", a.Kind)
	}
	fd, err := pathx.OpenBeneath(a.base, whole[:len(whole)-1],
		unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return -1, "", fmt.Errorf("opening the %s area: %w", a.Kind, err)
	}
	return fd, whole[len(whole)-1], nil
}

// make creates every directory from the base down to the named one, and
// returns a descriptor for the last.
//
// The intermediate directories get the ordinary 0755; the mode asked for
// belongs to the one the caller named, which is the one that carries a
// meaning -- the state directory is 0700 because of what is in it.
func (a Area) make(parts []string, mode os.FileMode) (int, error) {
	fd, err := unix.Open(a.base, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("opening %s: %w", a.base, err)
	}
	whole := a.below(parts...)
	for index, part := range whole {
		next, err := makeAt(fd, part, index == len(whole)-1, mode)
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

func makeAt(dir int, name string, last bool, mode os.FileMode) (int, error) {
	create := os.FileMode(0o755)
	if last {
		create = mode
	}
	if err := unix.Mkdirat(dir, name, uint32(create.Perm())); err != nil &&
		!errors.Is(err, unix.EEXIST) {
		return -1, fmt.Errorf("creating %s: %w", name, err)
	}
	fd, err := openAt(dir, name, unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		return -1, fmt.Errorf("opening %s: %w", name, err)
	}
	if last {
		// Put the mode back if it drifted, and undo whatever the umask took
		// off at creation.
		if err := unix.Fchmodat(dir, name, uint32(mode.Perm()), 0); err != nil {
			unix.Close(fd)
			return -1, fmt.Errorf("setting the mode of %s: %w", name, err)
		}
	}
	return fd, nil
}

// unlink removes one name below the area.
func (a Area) unlink(parts []string, flags int) error {
	dir, name, err := a.parent(parts)
	if err != nil {
		if isAbsent(err) {
			return nil
		}
		return err
	}
	defer unix.Close(dir)
	if err := unix.Unlinkat(dir, name, flags); err != nil && !isAbsent(err) {
		return fmt.Errorf("removing %s from the %s area: %w", name, a.Kind, err)
	}
	return nil
}

// openAt opens one name below a directory descriptor, following no
// symlink and never leaving that directory.
//
// This is the whole confinement, in one call: the kernel resolves the
// name and opens it in the same operation, so there is no moment between
// deciding and acting for anything to be swapped in.
func openAt(dir int, name string, flags int, mode os.FileMode) (int, error) {
	how := &unix.OpenHow{
		Flags:   uint64(flags) | unix.O_CLOEXEC,
		Mode:    uint64(mode.Perm()),
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_BENEATH,
	}
	fd, err := unix.Openat2(dir, name, how)
	if err != nil {
		switch {
		case errors.Is(err, unix.ELOOP):
			return -1, fmt.Errorf("%w: %s is a symbolic link", ErrOutside, name)
		case errors.Is(err, unix.EXDEV):
			return -1, fmt.Errorf("%w: %s", ErrOutside, name)
		}
		return -1, err
	}
	return fd, nil
}

// readNames lists a directory through a descriptor.
func readNames(dir int) ([]string, error) {
	duplicate, err := unix.Openat(dir, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), "directory")
	defer file.Close()
	return file.Readdirnames(-1)
}

func isAbsent(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR)
}
