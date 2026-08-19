package mountx

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/pathx"
)

// The graveyard is where a mount is taken to be unmounted.
//
// umount2 takes a path and the kernel resolves it again. That is the last
// hole in the privileged helper: the teardown decides *what* to remove by
// looking at a descriptor it resolved beneath the base it pinned, and then
// names the thing it decided about -- and the invoking user owns every
// directory above it, so between the decision and the call the name can be
// made to reach something else. Root then unmounts that.
//
// The obvious repair does not exist. Measured, C35: umount2 on a
// descriptor's own /proc/self/fd name fails in both directions -- a
// descriptor taken on the mount pins it, so the call answers EBUSY, and a
// descriptor taken on the mount point is resolved *through* into the
// mount, so it is the path that decided and not the descriptor. Holding a
// descriptor buys nothing at that call.
//
// What does work, measured as C36: open_tree names a mount by descriptor,
// move_mount moves that mount -- the same mount id arrives -- into a
// directory named by another descriptor, and umount2 removes it there once
// nothing pins it. So the *choice* is made on a descriptor and cannot be
// redirected, and the path the kernel finally resolves is one the invoking
// user cannot rename any part of. That path is what this is.
//
// # Where it is
//
// /run/camp/graveyard/<the helper's pid>, made by root, 0700, and gone
// with the next reboot -- which is exactly as long as a mount lasts.
//
// Not under the environment root, and that is the whole point: the
// invoking user owns the environment and normally its parent, so any name
// beneath it can be renamed away and replaced while the helper works. /run
// is root's, world-unwritable, and on every Linux that has an init. The pid
// makes two teardowns running at once two graveyards; nothing is shared
// between them.
//
// # Why it is a mount and not just a directory
//
// A mount attached under a *shared* parent is copied into that parent's
// peers, and the copies are not camp's to remove. /run is shared:5 on a
// systemd machine, so moving a composition into a plain directory there
// would put a copy of it in every peer namespace, for the moment it takes
// to unmount it -- and for good if the unmount failed. So the pid
// directory is bound onto itself and that bind made private, by
// descriptor, exactly as the staging and live points are and for the same
// kernel rule. It is the only mount camp makes outside an environment, it
// lives for one helper invocation, and Close takes it off.
//
// # The mounts that cannot come here
//
// A mount whose parent is shared cannot be moved at all -- the kernel
// answers EINVAL -- and camp's two self-binds are exactly that: their
// parent is the mount the environment root sits on, which is / , which is
// shared on a systemd machine. Detach exists because of that rule and
// cannot escape it for itself. So they have no graveyard route and never
// will.
//
// They come down through UnmountIn instead, which hands umount2 the
// parent directory's own /proc/self/fd name with the one component
// appended. The parent is then the directory a descriptor holds rather
// than whatever its name reaches, and C34 says the component below it
// cannot be renamed while something is mounted on it.
//
// This is not theory and the shape of it is worth keeping. Removing them
// by the recorded name was the residual this file was first written
// around, described as bounded because those names sit inside camp's own
// work directory. The rename race showed within one run what that missed:
// the whole environment root can be swapped for a link, so a name inside
// it reaches wherever the link points, and root unmounted a mount in a
// root-owned tree that was nothing to do with camp.
type Graveyard struct {
	// area is the pid directory, and the one thing here that writes.
	area fsx.Area
	// root is /run, resolved once and held, so the pid directory is
	// addressed from a descriptor like everything else camp writes.
	root pathx.Root
	// parts is the pid directory below that root, for opening slots.
	parts []string
	// bound says the self-bind stands and Close has to take it off.
	bound bool
	// slots counts the graves handed out; each mount gets its own, so one
	// that could not be removed cannot get in the way of the next.
	slots int

	opened bool
	failed error
}

// Where the graveyard lives, split out so a message and the walk agree.
//
// GraveyardBase is exported because a helper that cannot make one has to
// name it: the person reading that refusal needs to know which directory
// root could not write in.
const (
	GraveyardBase = "/run"
	graveyardDir  = "camp"
	graveyardName = "graveyard"
)

// NewGraveyard returns a graveyard that has not been made yet.
//
// Nothing is created and nothing is mounted until the first mount has to
// come down, so a helper that removes nothing -- a refused job, a teardown
// whose every recorded path is already clear -- never touches /run at all.
// That also keeps the unprivileged tests that drive the helper's teardown
// honest: they reach the same code and it asks root for nothing.
func NewGraveyard() *Graveyard { return &Graveyard{} }

// Open makes the graveyard if it is not there, and reports why not.
//
// A caller that must not act without it -- the teardown -- asks first and
// refuses on the answer, with nothing unmounted. Remove asks too, and
// falls back to the name if it cannot have one.
func (g *Graveyard) Open() error {
	if g.opened {
		return g.failed
	}
	g.opened = true
	g.failed = g.make()
	return g.failed
}

func (g *Graveyard) make() error {
	root, err := pathx.OpenRoot(GraveyardBase)
	if err != nil {
		return fmt.Errorf("opening %s, where camp takes a mount to unmount "+
			"it: %w", GraveyardBase, err)
	}
	g.root = root
	g.parts = []string{graveyardDir, graveyardName, strconv.Itoa(os.Getpid())}
	g.area = fsx.In("graveyard", root, g.parts...)
	if err := g.area.Ensure(0o700); err != nil {
		return fmt.Errorf("making %s, where camp takes a mount to unmount "+
			"it: %w", g.area.Root(), err)
	}

	fd, err := g.root.Open(g.parts, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return fmt.Errorf("opening %s: %w", g.area.Root(), err)
	}
	defer unix.Close(fd)

	// Recorded on what Detach reports and not on the call having succeeded:
	// the bind and the propagation change are two syscalls, and a graveyard
	// this could not finish still has to be taken off the machine by Close.
	standing, err := Detach(fd, g.area.Root())
	g.bound = standing
	if err != nil {
		return err
	}
	g.sweep()
	return nil
}

// sweep takes away the graveyards of helpers that are not running any
// more.
//
// A helper killed where it stands -- which is the state the kill matrix
// exists to produce, and the state a crash produces -- runs nothing
// deferred, so its graveyard is left mounted with nobody to remove it. No
// record names it: it is not part of any composition, it is in /run and
// not in an environment, and no teardown would ever be built with it as a
// target. Left alone it would accumulate one empty mount per killed
// helper.
//
// So each invocation clears what dead ones left. The name of a graveyard
// is the pid that made it, and a pid nothing answers for is a helper that
// is gone. A live pid is left alone, whether or not it is camp: the cost
// of being wrong in that direction is one directory nobody clears until
// next time, and in the other it is unmounting something a running helper
// is using.
//
// Best effort, and quiet. A graveyard that will not come away is one with
// a mount still in it -- somebody's composition, stranded by the kill --
// and that mount is in the machine's table under its own name, where a
// person looking for what is standing will find it. Failing this
// invocation over what a previous one left would be refusing to do the
// work in front of it because of work behind it.
func (g *Graveyard) sweep() {
	where := []string{graveyardDir, graveyardName}
	found, err := g.root.ReadDir(where)
	if err != nil {
		return
	}
	mine := strconv.Itoa(os.Getpid())
	for _, entry := range found {
		if entry.Name == mine || alive(entry.Name) {
			continue
		}
		stale := fsx.In("graveyard", g.root, append(where, entry.Name)...)
		if outcome, _ := Unmount(stale.Root()); outcome == Busy {
			continue
		}
		graves, err := g.root.ReadDir(append(where, entry.Name))
		if err != nil {
			continue
		}
		empty := true
		for _, grave := range graves {
			if stale.Remove(grave.Name) != nil {
				empty = false
			}
		}
		if empty {
			_ = stale.RemoveSelf()
		}
	}
}

// alive reports whether a graveyard's name is a process that still exists.
//
// Signal zero is the question without the signal: it performs the error
// checks and delivers nothing. Anything that is not a pid at all -- a
// name somebody put there by hand -- is treated as alive and left, because
// this removes what it can account for and nothing else.
func alive(name string) bool {
	pid, err := strconv.Atoi(name)
	if err != nil || pid <= 0 {
		return true
	}
	return unix.Kill(pid, 0) == nil
}

// Close takes the graveyard off the machine and says what is left.
//
// The order is the order it was made in, backwards: the graves first -- a
// grave still holding a mount cannot be removed and neither can the bind
// above it -- then the self-bind, then the directory. Anything it cannot
// clear is named, because a mount left in /run is a mount, and a teardown
// that reported a clean machine over one would be the one thing camp's
// failure handling may never do.
func (g *Graveyard) Close() error {
	if !g.opened || !g.root.Valid() {
		return nil
	}
	defer g.root.Close()

	var left []string
	for slot := 1; slot <= g.slots; slot++ {
		if err := g.area.Remove(strconv.Itoa(slot)); err != nil {
			left = append(left, fmt.Sprintf("%s/%d: %v", g.area.Root(), slot, err))
		}
	}
	if g.bound {
		// By name, and it is the one unmount here where that is not a
		// weakness: every directory above it belongs to root, so there is
		// nobody who can put something else at this name.
		if outcome, err := Unmount(g.area.Root()); outcome == Busy {
			left = append(left, fmt.Sprintf("%s: %v", g.area.Root(), err))
		} else {
			g.bound = false
		}
	}
	if len(left) == 0 {
		if err := g.area.RemoveSelf(); err != nil {
			left = append(left, fmt.Sprintf("%s: %v", g.area.Root(), err))
		}
	}
	if len(left) == 0 {
		return nil
	}
	return fmt.Errorf("camp moves a mount to %s to unmount it there, and could "+
		"not clear that afterwards:\n  %s\nWhat is named there is still mounted, "+
		"and root has to remove it", g.area.Root(), strings.Join(left, "\n  "))
}

// Mounted is one mount a caller is about to take down, addressed the way
// this can act on it.
//
// All three fields come out of one walk beneath the root the helper
// pinned, which is what makes them one mount rather than three chances to
// name a different one. FD is what the identity was checked on and what
// the handle is taken from. Dir and Name are the two the removal itself
// needs: where the mount goes back if it was moved out and then could not
// be removed, and -- for a mount that cannot be moved at all -- the only
// way to name it to umount2 without letting the kernel walk a path
// somebody else can change. Path is for a person reading a message, and
// is never what the kernel is given.
type Mounted struct {
	FD   int
	Dir  int
	Name string
	Path string
}

// Close releases whatever descriptors are left.
//
// A Mounted is a capability on one mount and on the directory it stands
// in, held only while the caller is deciding about that mount and taking
// it down. Remove closes the one on the mount itself and leaves -1 in its
// place, because holding that one across an unmount is what makes the
// unmount fail; the directory is the caller's until here.
func (m Mounted) Close() {
	for _, fd := range []int{m.FD, m.Dir} {
		if fd >= 0 {
			unix.Close(fd)
		}
	}
}

// release closes the descriptor on the mount and records that it is gone.
func (m *Mounted) release() {
	if m.FD >= 0 {
		unix.Close(m.FD)
		m.FD = -1
	}
}

// Remove takes down the mount a descriptor holds.
//
// The mount is named by that descriptor, moved into a grave only root can
// reach, and unmounted there. What the kernel resolves is the grave's
// path, and no part of that path is anybody else's to replace -- so a name
// swapped after the caller's identity check cannot redirect this.
//
// Two ways out of that route, and each has one honest answer:
//
//   - The mount cannot be moved. Its parent is shared, which is camp's two
//     self-binds on any systemd machine, and no move exists for them at
//     all. They come down through UnmountIn, on the directory descriptor
//     this walk already produced -- the file comment says why that is the
//     answer and what happened when it was a plain name.
//   - The move worked and the unmount did not, because something is still
//     inside. The mount goes back where it came from, addressed by the
//     directory descriptor and the one name, and the caller is told it is
//     busy -- exactly what it was told before, so the record, the holder
//     search and 'camp down' all keep meaning what they meant. A grave is
//     not a place a mount is left: the composition's own path is where its
//     user can see it, and where the next teardown looks.
//
// It takes the Mounted by pointer because it closes the descriptor on the
// mount and says so by setting it to -1. That is not tidiness: a
// descriptor on a mount is a reference to it, and umount2 answers EBUSY
// while any is held -- C35, and it is the same reference whoever holds it.
// The caller's descriptor is the one thing Remove cannot close by
// agreement, because the caller would then close a number the kernel had
// given to something else.
func (g *Graveyard) Remove(m *Mounted) (Outcome, error) {
	table, err := mountinfo.Read(mountinfo.Self)
	if err != nil || !movable(table, m.Path) {
		m.release()
		return UnmountIn(m.Dir, m.Name, m.Path)
	}

	// The mount itself, by descriptor. open_tree without OPEN_TREE_CLONE is
	// a handle on what is mounted at what the descriptor holds -- and the
	// descriptor was opened after the mount existed, so it is camp's mount
	// and not the directory underneath it.
	tree, err := unix.OpenTree(m.FD, "",
		unix.AT_EMPTY_PATH|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		m.release()
		return UnmountIn(m.Dir, m.Name, m.Path)
	}
	// Everything the caller's descriptor was for has been asked of it: it
	// decided which mount this is, and the handle that carries that
	// decision is taken. From here it is only a reference holding the
	// mount, so it goes -- before any unmount on either route.
	directory := directoryAt(m.FD)
	m.release()

	slot, slotFD, err := g.grave(directory)
	if err != nil {
		unix.Close(tree)
		return UnmountIn(m.Dir, m.Name, m.Path)
	}

	err = unix.MoveMount(tree, "", slotFD, "",
		unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH)
	// The handle and the grave, for the same reason and before the same
	// call.
	unix.Close(tree)
	unix.Close(slotFD)
	if err != nil {
		return UnmountIn(m.Dir, m.Name, m.Path)
	}

	outcome, failed := Unmount(slot)
	if outcome == Unmounted {
		return Unmounted, nil
	}
	if back := g.back(slot, *m); back != nil {
		return Busy, fmt.Errorf("%v.\n%v", failed, back)
	}
	return Busy, failed
}

// back puts a mount that would not come down where it stood.
//
// The handle is taken at the grave's own path, which is safe to resolve
// for the reason the whole graveyard is where it is: every directory above
// it is root's. The destination is not a path at all -- the directory the
// mount stood in, as the descriptor the caller opened it from, and the one
// name below it.
func (g *Graveyard) back(slot string, m Mounted) error {
	tree, err := unix.OpenTree(unix.AT_FDCWD, slot, unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return fmt.Errorf("it was moved to %s to be unmounted and camp could "+
			"not take hold of it there to put it back: %w", slot, err)
	}
	defer unix.Close(tree)

	if err := unix.MoveMount(tree, "", m.Dir, m.Name,
		unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("it was moved to %s to be unmounted, would not come "+
			"down, and could not be put back at %s: %w.\nIt is still mounted, at "+
			"%s, and root has to remove it", slot, m.Path, err, slot)
	}
	return nil
}

// movable reports whether the kernel will let the mount at a path be
// moved at all.
//
// It refuses two: a mount that is shared, and a mount whose parent is
// shared. The second is camp's two self-binds on any systemd machine --
// the environment root sits on / and / is shared:1 -- so they have no
// descriptor route and never had one. Asking the table before making a
// grave for them is what keeps an ordinary 'camp up' from building a
// graveyard it cannot use: the only mount that run removes is the staging
// self-bind, and that is exactly the unmovable kind.
//
// Unsure means yes. A table this cannot read, or a path it does not find,
// is not evidence that the descriptor route is unavailable -- and if it
// really is, move_mount says so and the fallback is the same one. Only a
// mount the table positively shows as shared skips it.
func movable(table []mountinfo.Entry, path string) bool {
	top, found := mountinfo.Top(table, path)
	if !found {
		return true
	}
	if top.Shared() {
		return false
	}
	for _, entry := range table {
		if entry.ID == top.Parent {
			return !entry.Shared()
		}
	}
	return true
}

// grave makes one mount its own place to be unmounted in, and answers with
// the path and a descriptor on it.
//
// The kind matters: move_mount puts a directory mount on a directory and a
// file mount on a file, and camp binds both -- a file island is a bind of
// one file. A grave of the wrong kind is a move the kernel refuses.
func (g *Graveyard) grave(directory bool) (string, int, error) {
	if err := g.Open(); err != nil {
		return "", -1, err
	}
	g.slots++
	name := strconv.Itoa(g.slots)

	var err error
	if directory {
		_, err = g.area.MkdirAllMode(0o700, name)
	} else {
		_, _, err = g.area.Touch(name)
	}
	if err != nil {
		return "", -1, err
	}
	path, err := g.area.Path(name)
	if err != nil {
		return "", -1, err
	}
	fd, err := g.root.Open(append(append([]string{}, g.parts...), name), unix.O_PATH)
	if err != nil {
		return "", -1, err
	}
	return path, fd, nil
}

// directoryAt answers what kind of thing a descriptor holds, and answers
// "directory" when it cannot tell.
//
// A mount that cannot be looked at is one the move is about to fail on
// anyway, and there the kernel gives the real reason.
func directoryAt(fd int) bool {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return true
	}
	return pathx.TypeOf(st.Mode) == pathx.Dir
}
