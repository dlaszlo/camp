package pathx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Root is a directory camp has resolved once and holds open.
//
// It exists for the one thing a strict walk below a base can never
// protect: the base itself. A base named by a string is resolved again in
// every call that uses it, so the owner of that directory can rename it
// away and leave a symlink at the old name between camp's check and
// camp's write -- and camp writes through the link, having refused a
// symlink at every component except the one it started from. A Root is
// resolved once and opened, and every later operation starts at the
// descriptor. The name it carries is a string for messages and for mount
// operands, and is never resolved a second time.
//
// It holds a pointer rather than a bare descriptor because Config and
// Area are copied by value all over camp, and 0 is a legal descriptor:
// standard input would silently become the base of an unpinned area. The
// zero value has to be detectably invalid instead, which is what Valid
// answers and what every operation refuses on.
type Root struct{ state *rootState }

// rootState is the descriptor and the name, shared by every copy of the
// Root that holds it -- so closing one closes them all, which is the
// truth about a descriptor and not a design choice.
type rootState struct {
	fd   int
	name string
}

// ErrNoRoot is what every operation on a Root that was never opened, or
// has been closed, fails with.
//
// It is a programming error rather than a user's, and it says so instead
// of acting: the alternative is acting on descriptor 0, which is whatever
// the process happens to be reading.
var ErrNoRoot = errors.New("camp holds no open directory for this area")

// OpenRoot resolves a path once and keeps the capability.
//
// Real is the deliberate symlink resolution this package's doc describes,
// and the descriptor is opened on what it resolved to, O_NOFOLLOW, so
// that the resolution and the descriptor cannot end up describing two
// different directories. Every question asked of the Root afterwards is
// answered from the descriptor, so the name can be renamed, replaced or
// linked over without changing where camp acts.
func OpenRoot(path string) (Root, error) {
	resolved, err := Real(path)
	if err != nil {
		return Root{}, err
	}
	fd, err := unix.Open(resolved,
		unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return Root{}, fmt.Errorf("opening %s: %w", resolved, err)
	}
	return Root{state: &rootState{fd: fd, name: resolved}}, nil
}

// OpenRootExactly opens the path it was given and resolves nothing.
//
// Which of the two constructors a caller wants is decided by where the
// path came from. A configuration's env: is written by a person, who may
// well have written it through a symlink and meant it, so OpenRoot
// resolves it once and holds what it resolved to. The privileged
// helper's base is not written by anybody: it arrives on a pipe from a
// front end that already resolved it. A symlink at its final component
// is therefore not a convenience but a replacement, made after the front
// end looked -- and root following it would mount this composition's
// operands beneath whatever the link points at. O_NOFOLLOW with
// O_DIRECTORY is that refusal, made by the kernel in the same call that
// opens.
//
// What this does not cover: every component above the final one is still
// resolved by the kernel here, by name, as any open resolves them. A base
// whose parent is a symlink is opened through it. Nothing about a name
// proves which directory a descriptor holds -- the check made on the
// descriptor afterwards is what decides, which for the helper is the
// ownership test, and it is made on this descriptor and on nothing that
// was looked at earlier.
func OpenRootExactly(path string) (Root, error) {
	fd, err := unix.Open(path,
		unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return Root{}, fmt.Errorf("opening %s: %w", path, err)
	}
	return Root{state: &rootState{fd: fd, name: path}}, nil
}

// Valid reports whether this Root holds a directory.
func (r Root) Valid() bool { return r.state != nil && r.state.fd >= 0 }

// Name is the path the Root resolved to, for a message or a mount
// operand. Nothing camp does resolves it again.
func (r Root) Name() string {
	if r.state == nil {
		return ""
	}
	return r.state.name
}

// Identity is the directory as the kernel knows it, taken from the
// descriptor -- so it answers for the directory camp holds and not for
// whatever the name points at now.
func (r Root) Identity() (Identity, error) {
	if err := r.held(); err != nil {
		return Identity{}, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(r.state.fd, &st); err != nil {
		return Identity{}, fmt.Errorf("looking at %s: %w", r.state.name, err)
	}
	return Identity{Device: uint64(st.Dev), Inode: st.Ino}, nil
}

// Current is the path this directory has now.
//
// A Root is the one thing camp holds that a rename cannot move: every
// operation starts at the descriptor, so the name it was opened by can be
// changed without changing where camp acts. That is the whole point of it
// -- and it leaves camp unable to notice that the name *was* changed,
// which matters wherever camp compares its own paths against something
// that reports paths of its own.
//
// The mount table is that something. It reports a mount at the path it is
// at now, and a mount point cannot be renamed (C34) while an ancestor
// can, so renaming the environment root moves every camp mount's reported
// path while camp goes on looking them up under the old one -- and "no
// mount is at that path" then answers a question about a name.
//
// /proc/self/fd/<descriptor> is the kernel's own answer, recomputed from
// the dentry tree at every read. A caller compares it against the path it
// believes this Root to be, and acts on the difference; nothing here
// decides what that difference means.
func (r Root) Current() (string, error) {
	if err := r.held(); err != nil {
		return "", err
	}
	return os.Readlink(fmt.Sprintf("/proc/self/fd/%d", r.state.fd))
}

// Close releases the descriptor. Closing twice is not an error, and every
// copy of the Root is closed with it.
func (r Root) Close() error {
	if !r.Valid() {
		return nil
	}
	fd := r.state.fd
	r.state.fd = -1
	return unix.Close(fd)
}

// Stat looks at parts below the root, following no symlink, including at
// the final component.
//
// With no parts it answers for the root itself, from the descriptor: the
// one thing about an area that cannot have been swapped since it was
// opened.
func (r Root) Stat(parts []string) (Info, error) {
	if err := r.held(); err != nil {
		return Info{}, err
	}
	if len(parts) == 0 {
		var st unix.Stat_t
		if err := unix.Fstat(r.state.fd, &st); err != nil {
			return Info{}, fmt.Errorf("looking at %s: %w", r.state.name, err)
		}
		return Info{
			Name:  filepath.Base(r.state.name),
			Type:  TypeOf(st.Mode),
			Ident: Identity{Device: uint64(st.Dev), Inode: st.Ino},
		}, nil
	}
	dir, err := openDirFrom(r.state.fd, r.state.name, parts[:len(parts)-1])
	if err != nil {
		if isAbsent(err) {
			return Info{Name: parts[len(parts)-1]}, nil
		}
		return Info{}, err
	}
	defer unix.Close(dir)
	return statAt(dir, parts[len(parts)-1], r.label(parts))
}

// Open opens parts below the root with the given flags, following no
// symlink and never leaving the root.
//
// With no parts it reopens the root itself through its own descriptor,
// which is how a caller gets a readable or lockable handle on a directory
// it holds only as O_PATH.
func (r Root) Open(parts []string, flags int) (int, error) {
	if err := r.held(); err != nil {
		return -1, err
	}
	if len(parts) == 0 {
		fd, err := unix.Openat(r.state.fd, ".", flags|unix.O_CLOEXEC, 0)
		if err != nil {
			return -1, fmt.Errorf("opening %s: %w", r.state.name, err)
		}
		return fd, nil
	}
	fd, dir, _, err := r.OpenIn(parts, flags)
	if err != nil {
		return -1, err
	}
	unix.Close(dir)
	return fd, nil
}

// OpenIn opens parts below the root and the directory holding the last of
// them, out of one walk, and returns that last component's name with
// them.
//
// The teardown needs all three about one mount. The mount itself is what
// its identity is checked on and what the handle that moves it is taken
// from. The directory it stands in and the name it stands under are where
// it goes back, if it was moved out and then could not be removed --
// addressed as a descriptor and a single name, so putting a mount back
// resolves nothing that anybody can replace in the meantime.
//
// Out of one walk, and not two, because two walks of one path are two
// resolutions of it, and this package exists so that there is one. The
// caller closes both descriptors.
//
// No parts is an error rather than an answer: the root's own directory is
// above where this package's confinement starts, and a caller asking for
// it is asking for a capability on something camp did not open.
func (r Root) OpenIn(parts []string, flags int) (fd, dir int, name string, err error) {
	if err := r.held(); err != nil {
		return -1, -1, "", err
	}
	if len(parts) == 0 {
		return -1, -1, "", fmt.Errorf("%s names nothing inside %s",
			r.state.name, r.state.name)
	}
	dir, err = openDirFrom(r.state.fd, r.state.name, parts[:len(parts)-1])
	if err != nil {
		return -1, -1, "", err
	}
	name = parts[len(parts)-1]

	how := &unix.OpenHow{
		Flags:   uint64(flags) | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_BENEATH,
	}
	fd, err = unix.Openat2(dir, name, how)
	if err != nil {
		unix.Close(dir)
		return -1, -1, "", translate(err, r.state.name, name)
	}
	return fd, dir, name, nil
}

// ReadDir lists parts below the root, in the byte order every other
// comparison camp makes uses. See ReadDirBeneath for why that order is
// not negotiable.
func (r Root) ReadDir(parts []string) ([]Info, error) {
	if err := r.held(); err != nil {
		return nil, err
	}
	fd, err := openDirFrom(r.state.fd, r.state.name, parts)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	return listDir(fd, r.label(parts))
}

// label renders the path of parts below this root, for a message only.
func (r Root) label(parts []string) string {
	return strings.Join(append([]string{r.state.name}, parts...), "/")
}

func (r Root) held() error {
	if !r.Valid() {
		return ErrNoRoot
	}
	return nil
}
