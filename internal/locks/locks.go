// Package locks guarantees one composition per upper and one per live
// directory.
//
// The danger is measured, not theoretical: the kernel happily mounts a
// second overlay on the same upper directory, and sharing an upper
// corrupts data. Neither the kernel nor an earlier design of this tool
// stopped it; this does.
//
// What is locked is not "the composition" but the directories taking part
// in it, and a composition is simply whoever holds that set of locks. The
// upper is exclusive, because one upper may serve one overlay. The live
// directory is exclusive, because it is the merged root and the work
// directory is keyed from it. Nothing else is locked: the lower, the
// record repository and the other mount sources are ordinary git-level
// parallelism.
//
// The lock is the directory's own inode -- flock on a descriptor for the
// directory itself. No lock file exists anywhere. That matters more than
// it sounds: a lock *file* under the environment directory meant that two
// environment directories naming the same upper by different paths locked
// two different inodes and neither saw the other. An inode cannot be
// missed that way -- every path to the same directory is the same lock,
// symlinks included.
//
// It also has to be something held rather than something written. In the
// namespace mode the other composition's mounts are invisible, so no
// mountinfo scan can see them; and a record file can go stale after
// kill -9, which would then need exactly the --force this design refuses.
// A flock is released by the kernel when the holder dies, so staleness is
// not reachable.
//
// Measured: a second flock on the same directory is
// refused; a flock through a symlink to it is refused as well, being the
// same inode; two different directories lock independently and one
// process holds both at once; the lock goes when the process dies; and
// locking writes nothing into the directory -- no entry appears, mtime
// and ctime are unchanged, which is why locking the code repository does
// not touch the invariant that camp never modifies a repository.
package locks

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/refusal"
)

// Role says which of the two directories a lock is on, for the message.
type Role string

const (
	// Upper is the code repository: one upper, one composition.
	Upper Role = "upper"
	// Live is the composed tree's directory.
	Live Role = "live"
)

// Held is a lock this process is holding.
type Held struct {
	Role Role
	Path string
	file *os.File
}

// FD returns the descriptor the lock lives on, so it can be handed to a
// child.
//
// A flock lives on the open file description, not on the descriptor, so
// an inherited copy carries the same lock -- which is what lets the
// launcher hand both locks to the process that will hold them for the
// session.
func (h *Held) FD() int { return int(h.file.Fd()) }

// File returns the open directory, for passing through exec.
func (h *Held) File() *os.File { return h.file }

// Release drops the lock and closes the descriptor.
func (h *Held) Release() {
	if h == nil || h.file == nil {
		return
	}
	h.file.Close()
	h.file = nil
}

// Pair is both locks, held together.
type Pair struct {
	Upper *Held
	Live  *Held
}

// Release drops both, live first -- the reverse of the order they were
// taken in.
func (p *Pair) Release() {
	if p == nil {
		return
	}
	p.Live.Release()
	p.Upper.Release()
}

// Files returns both descriptors, upper first, for handing to a child.
func (p *Pair) Files() []*os.File {
	if p == nil {
		return nil
	}
	return []*os.File{p.Upper.File(), p.Live.File()}
}

// TakePair takes both locks, upper first and live second, both
// non-blocking.
//
// Always in that order and never blocking, so two camps racing can only
// refuse each other: a deadlock is not reachable. If the second cannot be
// taken the first is released, so a refusal leaves nothing held.
func TakePair(base string, upperParts, liveParts []string, upperPath, livePath string) (*Pair, error) {
	upper, err := Take(Upper, base, upperParts, upperPath)
	if err != nil {
		return nil, err
	}
	live, err := Take(Live, base, liveParts, livePath)
	if err != nil {
		upper.Release()
		return nil, err
	}
	return &Pair{Upper: upper, Live: live}, nil
}

// Take locks one directory.
//
// The directory is opened beneath a base without following any symlink,
// because the thing that has to be locked is the directory the mounts
// will really use.
func Take(role Role, base string, parts []string, path string) (*Held, error) {
	fd, err := pathx.OpenBeneath(base, parts, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, refusal.New(string(role)+"-lock-unopenable",
			"the %s directory %s could not be opened to lock it: %v.", role, path, err)
	}
	file := os.NewFile(uintptr(fd), path)

	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if err == unix.EWOULDBLOCK {
			return nil, busy(role, path)
		}
		return nil, refusal.New(string(role)+"-lock-failed",
			"the %s directory %s could not be locked: %v.", role, path, err)
	}
	return &Held{Role: role, Path: path, file: file}, nil
}

// busy builds the refusal, naming what holds the directory.
func busy(role Role, path string) refusal.R {
	rule := "upper-locked"
	explanation := "One upper may serve one overlay. The kernel does not " +
		"enforce that -- a second overlay on the same upper mounts without " +
		"complaint -- and two compositions writing one upper corrupt each " +
		"other's data, so camp enforces it."
	if role == Live {
		rule = "live-locked"
		explanation = "Two compositions on one composed tree is not a thing " +
			"that can mean anything: the second would be laid over the first."
	}

	holders := Holders(path)
	var who string
	switch len(holders) {
	case 0:
		who = "camp could not find which process holds it. The lock is real -- " +
			"the kernel refused it -- so a process on this machine is holding " +
			"the directory open; it may belong to another user, whose open " +
			"files this process cannot see."
	case 1:
		who = fmt.Sprintf("It is held by pid %d: %s.", holders[0].PID, holders[0].Command)
	default:
		lines := make([]string, 0, len(holders))
		for _, holder := range holders {
			lines = append(lines, fmt.Sprintf("pid %d: %s", holder.PID, holder.Command))
		}
		who = "It is held by: " + strings.Join(lines, "; ") + "."
	}

	return refusal.New(rule,
		"a composition is already using %s.\n%s\n%s\n"+
			"The way in is to enter the composition that is running, not to "+
			"build a second one. If it was started with 'camp run -- tmux "+
			"new-session -d', attach to that tmux session from any terminal: "+
			"the client is only a pipe, and the windows it opens are children "+
			"of the server, which is inside. If the session is finished, it "+
			"releases this lock by itself when its last process exits -- the "+
			"kernel does that, so there is never a stale lock to clear and no "+
			"--force to reach for.",
		path, explanation, who)
}

// Holder is a process holding a flock.
type Holder struct {
	PID     int
	Command string
}

// Holders names the processes holding an flock on a directory.
//
// Two sources, because either one alone gets a session wrong.
//
// /proc/locks proves that a lock exists and on what: each FLOCK row
// carries the major:minor:inode of the locked object, so the directory's
// own stat identifies the row. But the pid in that row is the pid that
// *took* the lock, recorded when it was taken -- and camp deliberately
// hands the open file description to another process. In a namespace
// session the launcher takes the locks and the init inherits them, so by
// the time anybody asks, the row names a process that has already
// exited. It named a pid and then said "unknown", which is precisely the
// message this is not allowed to be.
//
// So the descriptors are scanned as well: a process holding the lock is
// holding a descriptor for that directory, and /proc/<pid>/fd says so.
// That finds whoever is really holding it now, under the pid this
// process can actually see.
//
// Nothing here parses any program's output, in any language.
func Holders(path string) []Holder {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return nil
	}
	if !locked(st) {
		return nil
	}
	return descriptorHolders(path, st)
}

// locked reports whether the kernel has an flock on this object.
func locked(st unix.Stat_t) bool {
	want := fmt.Sprintf("%02x:%02x:%d",
		unix.Major(uint64(st.Dev)), unix.Minor(uint64(st.Dev)), st.Ino)

	data, err := os.ReadFile("/proc/locks")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 6 && fields[1] == "FLOCK" && fields[5] == want {
			return true
		}
	}
	return false
}

// descriptorHolders finds the processes with the locked directory open.
func descriptorHolders(path string, st unix.Stat_t) []Holder {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}

	// This process is not excluded. It can never be the answer in a
	// refusal -- a second flock from the same process succeeds, so the
	// refusal is only ever reached about somebody else -- and leaving it
	// in makes the question the function actually answers the honest one:
	// who has this directory open.
	var holders []Holder
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if !holdsDescriptorFor(pid, st) {
			continue
		}
		holders = append(holders, Holder{PID: pid, Command: command(pid)})
	}
	sort.Slice(holders, func(i, j int) bool { return holders[i].PID < holders[j].PID })
	return holders
}

// holdsDescriptorFor reports whether a process has this exact directory
// open, compared by device and inode rather than by the descriptor's
// symlink text -- the same directory is reachable by more than one name,
// and a lock is on the inode.
func holdsDescriptorFor(pid int, st unix.Stat_t) bool {
	directory := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		var open unix.Stat_t
		if err := unix.Stat(filepath.Join(directory, entry.Name()), &open); err != nil {
			continue
		}
		if open.Dev == st.Dev && open.Ino == st.Ino {
			return true
		}
	}
	return false
}

func command(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err == nil {
		text := strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
		if text != "" {
			if len(text) > 80 {
				text = text[:80]
			}
			return text
		}
	}
	if comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
		return strings.TrimSpace(string(comm))
	}
	return "unknown"
}

// Adopt rebuilds a Held from a descriptor inherited through exec.
//
// The lock is already on the open file description; this only gives the
// process a handle to keep it alive and to release it deliberately.
func Adopt(role Role, path string, fd int) *Held {
	return &Held{Role: role, Path: path, file: os.NewFile(uintptr(fd), path)}
}
