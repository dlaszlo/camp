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
// **An area is a pinned root and the components below it, and every one
// of those components is resolved by the kernel, in the call that acts on
// it, following no symlink and never leaving the root.** That is what
// makes the invariant true rather than merely intended. Joining strings
// and calling MkdirAll would follow whatever a symlink at $ENV/.camp/work
// pointed at -- into a repository, if somebody put it there -- and no
// check made beforehand could help, because the check and the write would
// be two resolutions of the same name with a gap between them. There is
// no gap here: openat2 resolves and acts at once, and refuses a symlink
// and an escape itself.
//
// The root is the one thing camp trusts, and it is a descriptor, not a
// string. The environment root is resolved once, when the configuration
// is read, and held open for the whole command; the state directory and
// the probe's scratch tree are resolved once each in the same way. A root
// kept as a name would be resolved again at every write, and its owner
// can rename it away and leave a symlink at the old name between camp's
// validation and camp's write -- which is the whole class of attack the
// strict walk below it already refuses. Everything below the root is
// camp's own and is resolved the strict way.
//
// The one thing camp deletes outside these areas is the kernel's own
// leftover work directory inside its work area, which is inside work
// anyway.
package fsx

import (
	"crypto/rand"
	"encoding/hex"
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
	// root is the directory camp trusts, held open, and parts are the
	// components below it that make up this area. They are kept apart
	// because the root is the point resolution starts from and never
	// leaves -- and it is a descriptor rather than a string because a
	// string is resolved again in every call that uses it, which is the
	// one place a symlink could still be swapped in.
	root  pathx.Root
	parts []string
}

// ErrOutside is returned when a path would leave its area. It is a
// programming error, never a user's, and it fails loudly rather than
// writing somewhere plausible.
var ErrOutside = errors.New("the path leaves the area camp may write in")

// ErrChangedType is returned when the object at a name stopped being what
// it was between camp looking at it and camp acting on what it saw.
//
// It exists because the alternative was reporting success. A removal that
// met a file where it had found a directory, or a chown that met the
// reverse, did not do what it was asked, and a cleanup that says a name
// is clear while a replacement stands at it is the one answer nothing
// downstream can recover from.
var ErrChangedType = errors.New("the object changed type while camp was working on it")

// ErrExists is returned by the calls that publish a name only if nothing
// holds it, so a caller can try another name rather than read a refusal.
var ErrExists = errors.New("something already holds that name")

// raceAttempts bounds how often something else's interference may make a
// step start again -- a type change under a removal, a temporary name
// already taken.
//
// Every pass acts on what is at the name now, so an ordinary race --
// something finishing a rename while camp walks past it -- settles on the
// first or second pass. A name somebody flips on purpose never settles,
// and an unbounded loop would hand that person this process for as long
// as they cared to keep flipping. Past the bound camp says what it met,
// which is an answer a retry could not have reached.
const raceAttempts = 8

// afterTypeCheck runs between deciding what an object is and acting on
// that decision.
//
// It does nothing in a running camp. It is here so a test can replace a
// directory with a file, and a file with a directory, in exactly the
// window where the swap would otherwise be unreachable from a test -- the
// way doctorTable in internal/cli exists for a mount table nobody can
// arrange on purpose. It can only add work, never skip a step.
var afterTypeCheck = func(name string) {}

// afterTemporaryOpen runs between creating the temporary an atomic write
// builds in and writing anything into it.
//
// The same seam, for the same reason: the two-writer schedule this
// package is written against -- both writers holding an open temporary at
// once -- has no other way to be arranged from a test.
var afterTemporaryOpen = func() {}

// failStep can make one step of an atomic write fail, so that the callers
// which have to survive a half-finished publication can be measured
// against every place it can stop.
//
// It returns nil in a running camp, and it can only add a failure: no
// step is skipped because of it, so the worst a mistake here can do is
// refuse a write that would have succeeded.
var failStep = func(step string) error { return nil }

// In builds an area from a root camp holds open and the components below
// it.
//
// The root is where the confinement starts, so a caller passes the
// highest directory it has already established is not a repository and
// not reached through anybody's symlink -- the environment root, for
// everything under .camp. It is passed already open because opening it
// can fail, and a constructor that could fail would put that failure at
// every place an area is built rather than at the handful of places where
// a root is resolved.
func In(kind string, root pathx.Root, parts ...string) Area {
	return Area{Kind: kind, root: root, parts: append([]string(nil), parts...)}
}

// Work is the disposable area for one composition.
func Work(root pathx.Root, hash string) Area {
	return In("work", root, config.Dir, "work", hash)
}

// Storage is the persistent area for one composition. camp never removes
// it: it holds unfinished worktrees and machine-local state.
func Storage(root pathx.Root, hash string) Area {
	return In("storage", root, config.Dir, "storage", hash)
}

// State is where the privileged mode's records live. Its root is the
// user's own state directory, which is not camp's to vouch for; camp's
// own directory below it is.
func State(root pathx.Root, name string) Area { return In("state", root, name) }

// Reports is where a namespace session leaves its end-of-session report.
func Reports(root pathx.Root) Area { return In("reports", root, config.Dir, "reports") }

// Logs is where camp keeps what it said. It is the one area written by
// appending rather than by replacing: a log is a record of a sequence,
// and rewriting the file to add a line to it would lose the sequence at
// every crash.
func Logs(root pathx.Root) Area { return In("logs", root, config.Dir, "logs") }

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
func Live(root pathx.Root, parts ...string) Area { return In("live", root, parts...) }

// Scratch makes a directory that belongs to nobody else and returns it as
// an area, with the call that removes it again.
//
// For the capability probe: it builds a real overlay somewhere harmless
// to find out whether this machine can, inside a namespace that vanishes
// with the process. It is still camp writing to a filesystem, so it goes
// through the one door like everything else -- and because the door is
// the only place that writes, the probe's tree cannot be anywhere near a
// repository either.
//
// The base is /tmp and never os.TempDir(), which honours $TMPDIR: a person
// may have pointed that at a repository, and the invariant is that camp
// writes into no repository, not that it usually removes what it wrote
// afterward. /tmp is the fixed, non-repository place the probe already used
// whenever $TMPDIR was unset. The cleanup returns its failure rather than
// dropping it, because a scratch tree left behind is still a write.
func Scratch(prefix string) (Area, func() error, error) {
	directory, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		return Area{}, func() error { return nil }, fmt.Errorf("making a scratch directory: %w", err)
	}
	root, err := pathx.OpenRoot(directory)
	if err != nil {
		_ = removeTree(directory)
		return Area{}, func() error { return nil }, fmt.Errorf("making a scratch directory: %w", err)
	}
	area := Area{Kind: "scratch", root: root}
	return area, func() error {
		err := removeTree(directory)
		if closed := root.Close(); err == nil {
			err = closed
		}
		return err
	}, nil
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
func Camp(root pathx.Root) Area { return In("camp", root, config.Dir) }

// OpenDir opens the area's own directory, resolved from the root the way
// everything else here is.
//
// For the callers that need the directory itself rather than something in
// it: a flock is on an inode, and a descriptor is the only thing that
// holds one. Nothing is written through it -- an area is still the only
// way camp writes -- and the caller closes it.
func (a Area) OpenDir() (*os.File, error) {
	if !a.root.Valid() {
		return nil, fmt.Errorf("an empty %s area was used", a.Kind)
	}
	fd, err := a.root.Open(a.parts, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, fmt.Errorf("opening the %s area: %w", a.Kind, err)
	}
	return os.NewFile(uintptr(fd), a.Root()), nil
}

// Root returns the area's own directory, as a path.
//
// For messages, and for handing to the kernel as a mount operand. camp's
// own writes never go through it: they are addressed by component from
// the root camp holds open, which is the whole point of the type.
func (a Area) Root() string {
	return filepath.Join(append([]string{a.root.Name()}, a.parts...)...)
}

// Path resolves a relative path inside the area, refusing anything that
// would climb out of it.
//
// The same caveat as Root: what comes back is a name for a message or an
// operand for the kernel, and never the way camp writes.
func (a Area) Path(parts ...string) (string, error) {
	if !a.root.Valid() {
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
	return Area{Kind: a.Kind, root: a.root, parts: a.below(parts...)}, nil
}

// Ensure creates the area's own directory, and every directory above it
// down from the root, with a mode of its own for the area itself.
//
// The state directory is 0700 and its records are 0600: a record names
// every path of a composition, and that is nobody else's business.
func (a Area) Ensure(mode os.FileMode) error {
	if !a.root.Valid() {
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
		return fmt.Errorf("the %s area's own root is not camp's to remove", a.Kind)
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
//
// A source that is not there is an error like any other. It used to be
// folded into success, which cost two things: the loser of a race to
// perform a once-only transition was told it had performed it, and log
// rotation could not tell an old generation that has never existed from
// the current file disappearing under it. A caller that expects a
// particular absence says so at its own call site, where it knows which
// absence it means.
func (a Area) Rename(from, to string) error {
	return a.renameWith(from, to, 0)
}

// RenameNew is Rename that refuses to replace anything: the destination
// is claimed only if nothing holds it, in the same step that moves the
// source onto it.
//
// RENAME_NOREPLACE is what makes it one step. Looking first and renaming
// afterwards is two resolutions of the destination with a gap between
// them, and the gap is where the file somebody kept gets overwritten by
// the rename that was told the name was free.
func (a Area) RenameNew(from, to string) error {
	return a.renameWith(from, to, unix.RENAME_NOREPLACE)
}

func (a Area) renameWith(from, to string, flags uint) error {
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

	if err := unix.Renameat2(fromDir, fromName, toDir, toName, flags); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("renaming %s to %s in %s: %w", from, to, a.Kind, ErrExists)
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
	return a.write(name, data, mode, 0)
}

// WriteNew publishes a file that must not already be there: the name
// appears, complete, or it does not appear at all.
//
// For the callers whose file is the whole of what it says -- a report a
// session left behind is read once and then marked, so a name that exists
// while the body is still being written is a delivery of nothing. A name
// something else already holds comes back as ErrExists, for a caller that
// has another name to try.
//
// The claim and the publication are one syscall, RENAME_NOREPLACE, which
// the kernel has had for far longer than the openat2 this package is
// built on. A filesystem that does not implement the flag refuses the
// rename rather than replacing anything, so the failure is a report that
// was not written and never a report that was overwritten.
func (a Area) WriteNew(name string, data []byte, mode os.FileMode) error {
	return a.write(name, data, mode, unix.RENAME_NOREPLACE)
}

func (a Area) write(name string, data []byte, mode os.FileMode, flags uint) error {
	if err := a.usable([]string{name}); err != nil {
		return err
	}
	dir, last, err := a.parent([]string{name})
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	return writeAt(dir, last, data, mode, a.Kind, flags)
}

// writeAt is the atomic replacement, performed entirely through a
// directory descriptor so that no part of it re-resolves a path.
//
// The temporary is unique to this call and created with O_EXCL. One fixed
// name per destination meant two processes replacing the same file opened
// the same inode: the first wrote, synced and renamed it into place, and
// the second then wrote through its own descriptor -- which by then
// referred to the published file -- so a successful atomic write changed
// after it had returned, and the bytes at the destination were half of
// each payload. Nothing here shares a name with another writer any more,
// and the cleanup unlinks only the temporary this call made.
func writeAt(dir int, name string, data []byte, mode os.FileMode, kind string, flags uint) error {
	temporary, fd, err := createTemporary(dir, name, mode)
	if err != nil {
		return fmt.Errorf("writing %s in %s: %w", name, kind, err)
	}
	file := os.NewFile(uintptr(fd), temporary)
	published := false
	defer func() {
		file.Close()
		// Only until it is published. After the rename this name holds
		// nothing, and unlinking it would be a guess about what does.
		if !published {
			unix.Unlinkat(dir, temporary, 0)
		}
	}()

	afterTemporaryOpen()

	if err := failStep("write"); err != nil {
		return fmt.Errorf("writing %s in %s: %w", name, kind, err)
	}
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("writing %s in %s: %w", name, kind, err)
	}
	if err := failStep("chmod"); err != nil {
		return fmt.Errorf("setting the mode of %s in %s: %w", name, kind, err)
	}
	if err := file.Chmod(mode); err != nil {
		return fmt.Errorf("setting the mode of %s in %s: %w", name, kind, err)
	}
	if err := failStep("sync"); err != nil {
		return fmt.Errorf("syncing %s in %s: %w", name, kind, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("syncing %s in %s: %w", name, kind, err)
	}
	if err := failStep("rename"); err != nil {
		return fmt.Errorf("replacing %s in %s: %w", name, kind, err)
	}
	if err := unix.Renameat2(dir, temporary, dir, name, flags); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("writing %s in %s: %w", name, kind, ErrExists)
		}
		return fmt.Errorf("replacing %s in %s: %w", name, kind, err)
	}
	published = true
	return syncFd(dir)
}

// nameMax is the longest name a directory entry may have. Every
// filesystem camp writes to enforces it, and the temporary has to fit
// beside the destination's own name.
const nameMax = 255

// createTemporary makes the file an atomic write is built in, under a
// name nothing else can be using.
//
// Unpredictable rather than merely unique: the random part means another
// process cannot arrange to hold this name before camp asks for it, and
// O_EXCL means camp finds out rather than adopting whatever is there. The
// same bound as a type race, for the same reason -- sixty-four random
// bits do not collide by chance, so a name that keeps coming back taken
// is somebody putting it there, and camp says so instead of trying for
// ever.
func createTemporary(dir int, name string, mode os.FileMode) (string, int, error) {
	var last error
	for attempt := 0; attempt < raceAttempts; attempt++ {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", -1, fmt.Errorf("naming a temporary file: %w", err)
		}
		suffix := "." + hex.EncodeToString(raw[:]) + ".camp"
		stem := name
		if room := nameMax - 1 - len(suffix); len(stem) > room {
			stem = stem[:room]
		}
		temporary := "." + stem + suffix

		fd, err := openAt(dir, temporary, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY, mode)
		if err == nil {
			return temporary, fd, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return "", -1, err
		}
		last = err
	}
	return "", -1, last
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
//
// Two calls, because one name can be either kind of object and the kernel
// takes a different flag for each. A name that has become the other kind
// between them comes back as the kernel's own type error rather than as
// success: this used to end in a suppressed ENOTDIR, which is a removal
// reported over a file that is still there.
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

// removeTreeAt makes one name absent, starting again when the object at
// it changes type under the walk.
//
// A retry rather than a refusal, because what this is asked for is that
// the name be gone, and that is a conclusion a second pass can reach: it
// looks again and removes whatever is there now. The chown below decides
// the other way, for the reason given there. The bound is what stops a
// name being flipped on purpose from holding this here for ever, and past
// it camp says what it met rather than reporting a removal it did not
// make.
func removeTreeAt(dir int, name string) error {
	for attempt := 0; attempt < raceAttempts; attempt++ {
		err := removeTreeOnce(dir, name)
		if !errors.Is(err, ErrChangedType) {
			return err
		}
	}
	return fmt.Errorf("removing %s: %w", name, ErrChangedType)
}

// removeTreeOnce empties and removes one name as the kind of object it
// found there. Meeting the other kind is ErrChangedType and never
// success: the name is still held, by something else than a moment ago.
func removeTreeOnce(dir int, name string) error {
	// Opened before anything is decided about it, following no symlink at
	// the final name, so the type check and the mode change below act on
	// this one inode and never on the name -- which the owner can point
	// elsewhere between two syscalls.
	fd, err := unix.Openat(dir, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if isAbsent(err) {
			return nil
		}
		return fmt.Errorf("looking at %s: %w", name, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return fmt.Errorf("looking at %s: %w", name, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		unix.Close(fd)
		afterTypeCheck(name)
		if err := unix.Unlinkat(dir, name, 0); err != nil {
			switch {
			case isAbsent(err):
				return nil
			case errors.Is(err, unix.EISDIR):
				// A directory stands where the file was. Nothing was removed,
				// so this must not answer that the name is clear.
				return fmt.Errorf("%s: %w", name, ErrChangedType)
			}
			return fmt.Errorf("removing %s: %w", name, err)
		}
		return nil
	}

	// The kernel's leftover is mode 000; without this it cannot even be
	// listed, let alone emptied. Changed through the descriptor opened
	// above, so a symlink swapped in at the name after the type check is
	// changed on nothing: this descriptor holds the directory itself.
	if st.Mode&0o300 != 0o300 {
		if err := chmodFd(fd, 0o700); err != nil {
			unix.Close(fd)
			return fmt.Errorf("making %s removable: %w", name, err)
		}
	}
	unix.Close(fd)
	afterTypeCheck(name)
	child, err := openAt(dir, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		switch {
		case isAbsent(err):
			return nil
		case errors.Is(err, unix.ENOTDIR):
			// A file stands where the directory was. The old code read this
			// as absence and returned success over the replacement.
			return fmt.Errorf("%s: %w", name, ErrChangedType)
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
	if err := unix.Unlinkat(dir, name, unix.AT_REMOVEDIR); err != nil {
		switch {
		case isAbsent(err):
			return nil
		case errors.Is(err, unix.ENOTDIR):
			return fmt.Errorf("%s: %w", name, ErrChangedType)
		}
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

// chownTreeAt gives one name and everything under it to a user, and
// refuses rather than retrying when the name stops being the directory it
// was walking.
//
// The other way from the removal above, and for a reason rather than for
// symmetry. A removal is asked to make a name absent, so a second pass on
// whatever is there now still reaches what it was asked for. This is
// asked to give a known subtree to a user: if the directory it had
// identified is gone, a second pass would be giving away whatever
// replaced it, which is a different act with the same name. The caller is
// the privileged helper, running as root over the one directory the
// kernel made, and handing a stranger's object to somebody is not the
// operation it went there to perform.
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
	afterTypeCheck(name)
	child, err := openAt(dir, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		switch {
		case isAbsent(err):
			return nil
		case errors.Is(err, unix.ENOTDIR):
			// A file stands where the directory was, so nothing below it was
			// reached. The old code read this as absence and reported the whole
			// subtree given away.
			return fmt.Errorf("giving %s to uid %d: %w", name, uid, ErrChangedType)
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
	if !a.root.Valid() {
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
// the root downwards, and returns it with that name.
func (a Area) parent(parts []string) (int, string, error) {
	whole := a.below(parts...)
	if len(whole) == 0 {
		return -1, "", fmt.Errorf("the %s area's own root is not camp's to write", a.Kind)
	}
	fd, err := a.root.Open(whole[:len(whole)-1], unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return -1, "", fmt.Errorf("opening the %s area: %w", a.Kind, err)
	}
	return fd, whole[len(whole)-1], nil
}

// make creates every directory from the root down to the named one, and
// returns a descriptor for the last.
//
// The intermediate directories get the ordinary 0755; the mode asked for
// belongs to the one the caller named, which is the one that carries a
// meaning -- the state directory is 0700 because of what is in it.
func (a Area) make(parts []string, mode os.FileMode) (int, error) {
	fd, err := a.root.Open(nil, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return -1, err
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
		// off at creation. Through the descriptor just opened, not the name:
		// a symlink swapped in at the name after the open is changed on
		// nothing.
		if err := chmodFd(fd, mode); err != nil {
			unix.Close(fd)
			return -1, fmt.Errorf("setting the mode of %s: %w", name, err)
		}
	}
	return fd, nil
}

// chmodFd changes the mode of exactly the inode a descriptor holds.
//
// The descriptor is opened O_PATH, which fchmod cannot act on directly, so
// the change is addressed through the descriptor's own /proc/self/fd name.
// That name resolves to the inode the descriptor holds, whatever the
// original name points at now -- which is the whole point: a symlink
// swapped in at the name after the type was checked is changed on nothing.
func chmodFd(fd int, mode os.FileMode) error {
	return unix.Chmod(fmt.Sprintf("/proc/self/fd/%d", fd), uint32(mode.Perm()))
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

// isAbsent is the one error that means the name is simply not there.
//
// ENOTDIR is not in it, and neither is EISDIR. Both say the name is held
// -- by something other than what camp had just looked at -- and folding
// them into absence is how a removal or a chown came to report success
// over a replacement it never touched. Absence here means ENOENT, the
// same as in pathx, and every other type conflict is answered where it
// arises.
func isAbsent(err error) bool {
	return errors.Is(err, unix.ENOENT)
}
