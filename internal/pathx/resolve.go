package pathx

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// Type is what a name is, without following it.
type Type string

const (
	// Absent means the name is not there.
	Absent Type = ""
	// Dir is a directory.
	Dir Type = "directory"
	// File is a regular file.
	File Type = "file"
	// Symlink is a symbolic link. It is a type camp reports and refuses,
	// never one it follows.
	Symlink Type = "symlink"
	// Socket, FIFO and Device are the remaining kinds a directory can
	// hold. camp mounts none of them, and names the kind when it refuses,
	// so the message can say what changed.
	Socket Type = "socket"
	FIFO   Type = "named pipe"
	Device Type = "device"
)

// Info is one name, looked at without following it.
type Info struct {
	Name  string
	Type  Type
	Ident Identity
	// Link is the symlink's target, when Type is Symlink. It may contain
	// anything a Linux path may contain, newlines included, which is why
	// every record camp writes it into is escaped.
	Link string
}

// Exists reports whether the name is there at all.
func (i Info) Exists() bool { return i.Type != Absent }

// ErrSymlinkInPath is returned when a component of the path being
// resolved is a symbolic link.
//
// Not followed, ever. A bind mount follows symlinks, so a symlink
// anywhere in a mount operand could pull an arbitrary directory on the
// machine into the composition -- and between the moment camp validates a
// path and the moment it is mounted, a component the user owns can be
// swapped for one. Refusing the whole class is the only check that does
// not have a race inside it.
var ErrSymlinkInPath = errors.New("a component of the path is a symbolic link")

// ErrEscapes is returned when resolution would leave the base directory.
var ErrEscapes = errors.New("the path leaves the directory it is resolved against")

// openDirBeneath opens base and then walks parts, refusing to follow any
// symlink and refusing to leave base.
//
// The last component is deliberately not opened: lstat of the final name
// is what the caller wants, and opening it would mean following it.
func openDirBeneath(base string, parts []string) (int, error) {
	fd, err := unix.Open(base, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("opening %s: %w", base, err)
	}
	for _, part := range parts {
		how := &unix.OpenHow{
			Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_BENEATH,
		}
		next, err := unix.Openat2(fd, part, how)
		unix.Close(fd)
		if err != nil {
			return -1, translate(err, base, part)
		}
		fd = next
	}
	return fd, nil
}

func translate(err error, base, part string) error {
	switch {
	case errors.Is(err, unix.ELOOP):
		return fmt.Errorf("%w: %s in %s", ErrSymlinkInPath, part, base)
	case errors.Is(err, unix.EXDEV):
		return fmt.Errorf("%w: %s in %s", ErrEscapes, part, base)
	default:
		return err
	}
}

// StatBeneath looks at base/parts without following a symlink anywhere,
// including the final component.
//
// An absent name is not an error: it returns Info{Type: Absent}, because
// "is it there" is the question most callers are asking and a missing
// name is an ordinary answer to it.
func StatBeneath(base string, parts []string) (Info, error) {
	if len(parts) == 0 {
		return statAt(unix.AT_FDCWD, base, base)
	}
	dir, err := openDirBeneath(base, parts[:len(parts)-1])
	if err != nil {
		if isAbsent(err) {
			return Info{Name: parts[len(parts)-1]}, nil
		}
		return Info{}, err
	}
	defer unix.Close(dir)
	return statAt(dir, parts[len(parts)-1], strings.Join(append([]string{base}, parts...), "/"))
}

func statAt(dirfd int, name, full string) (Info, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if isAbsent(err) {
			return Info{Name: name}, nil
		}
		return Info{}, fmt.Errorf("looking at %s: %w", full, err)
	}

	info := Info{
		Name:  name,
		Type:  typeOf(st.Mode),
		Ident: Identity{Device: uint64(st.Dev), Inode: st.Ino},
	}
	if info.Type == Symlink {
		buffer := make([]byte, unix.PathMax)
		n, err := unix.Readlinkat(dirfd, name, buffer)
		if err == nil {
			info.Link = string(buffer[:n])
		}
	}
	return info, nil
}

func isAbsent(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR)
}

func typeOf(mode uint32) Type {
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return Dir
	case unix.S_IFREG:
		return File
	case unix.S_IFLNK:
		return Symlink
	case unix.S_IFSOCK:
		return Socket
	case unix.S_IFIFO:
		return FIFO
	case unix.S_IFBLK, unix.S_IFCHR:
		return Device
	default:
		return Type("unrecognised object")
	}
}

// ReadDirBeneath lists base/parts, following no symlink on the way and
// reporting each entry's own type without following it either.
//
// Sorted by name bytes, never by locale: a locale sort and a byte sort
// silently disagree, and every comparison camp makes -- the gate, the
// inventory, the exclude -- has to be the same order or two of them will
// quietly describe different sets.
func ReadDirBeneath(base string, parts []string) ([]Info, error) {
	fd, err := openDirBeneath(base, parts)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)

	names, err := readNames(fd, strings.Join(append([]string{base}, parts...), "/"))
	if err != nil {
		return nil, err
	}

	entries := make([]Info, 0, len(names))
	for _, name := range names {
		info, err := statAt(fd, name, name)
		if err != nil {
			return nil, err
		}
		if info.Exists() {
			entries = append(entries, info)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// readNames lists a directory through a second descriptor, because an
// O_PATH descriptor cannot be read from.
func readNames(dirfd int, label string) ([]string, error) {
	duplicate, err := unix.Openat(dirfd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", label, err)
	}
	file := os.NewFile(uintptr(duplicate), label)
	defer file.Close()
	return file.Readdirnames(-1)
}
