package probe

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Tree is the content of a directory, as one hash.
//
// The assertion it serves is "the repositories and the storage hash
// identically before and after", and what has to be caught is a write
// camp made where it promised to make none. So this hashes what a write
// would change: every name in byte order, what kind of thing it is, its
// permission bits, and for a regular file its bytes, for a symbolic link
// its target.
//
// What it deliberately leaves out is time. mtime moves when a directory
// is read on a relatime filesystem, and a driver that failed on that
// would fail on every run for a reason that is not a write.
//
// A tree that is not there hashes to the empty string rather than
// failing: camp's storage does not exist until the first composition
// makes it, and "it was not there before and is not there now" is a pair
// of equal answers.
func Tree(root string) (string, error) {
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return "", nil
	}
	sum := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(sum, "%s\x00%s\x00%o\x00", relative, kind(info.Mode()), info.Mode().Perm())

		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(sum, "%s\x00", target)
		case info.Mode().IsRegular():
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(sum, file)
			file.Close()
			if err != nil {
				return err
			}
			fmt.Fprintf(sum, "\x00")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func kind(mode fs.FileMode) string {
	switch {
	case mode.IsDir():
		return "directory"
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	case mode.IsRegular():
		return "file"
	default:
		return mode.Type().String()
	}
}

// Modes is every inode's permission bits beneath a directory, by path.
//
// The rename race asserts that no inode mode outside the original base
// changes, and a hash cannot say *which* one moved. This can.
func Modes(root string) (map[string]fs.FileMode, error) {
	modes := map[string]fs.FileMode{}
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return modes, nil
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		modes[path] = info.Mode()
		return nil
	})
	return modes, err
}
