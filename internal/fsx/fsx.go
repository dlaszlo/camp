// Package fsx is the only place in camp that writes to a filesystem.
//
// That is not a tidiness preference. The first invariant -- camp only
// composes, it never modifies a repository -- is a property of the source
// code, and the way to make it checkable is to have one door and to be
// able to say what is behind it. Every create, write, chmod and remove
// camp performs goes through this package, and every one of them is
// addressed relative to an Area: one of the four places camp owns.
//
//	work     $ENV/.camp/work/<hash>      disposable, swept when nothing is mounted
//	storage  $ENV/.camp/storage/<hash>   persistent, never removed by camp
//	state    $XDG_STATE_HOME/camp        the privileged mode's records
//	reports  $ENV/.camp/reports          what a namespace session leaves behind
//
// An Area refuses any path that would leave it, so no write target can be
// derived from a repository path even by accident: there is no
// constructor that takes one. A repository path cannot become an Area,
// which is the property the invariant needs.
//
// The one thing camp deletes outside these four is the kernel's own
// leftover work directory inside its work area, which is inside work
// anyway.
package fsx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Area is a directory camp owns, and the root of everything it may write.
type Area struct {
	// Kind names the area for a message: "work", "storage", "state",
	// "reports".
	Kind string
	root string
}

// ErrOutside is returned when a path would leave its area. It is a
// programming error, never a user's, and it fails loudly rather than
// writing somewhere plausible.
var ErrOutside = errors.New("the path leaves the area camp may write in")

// Work is the disposable area for one composition.
func Work(root string) Area { return Area{Kind: "work", root: root} }

// Storage is the persistent area for one composition. camp never removes
// it: it holds unfinished worktrees and machine-local state.
func Storage(root string) Area { return Area{Kind: "storage", root: root} }

// State is where the privileged mode's records live.
func State(root string) Area { return Area{Kind: "state", root: root} }

// Reports is where a namespace session leaves its end-of-session report.
func Reports(root string) Area { return Area{Kind: "reports", root: root} }

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
func Live(root string) Area { return Area{Kind: "live", root: root} }

// Camp is $ENV/.camp itself: the configuration, and the two stores below
// it. Only 'camp init' writes here, and only the configuration skeleton,
// which is the one file a person asked camp to create.
func Camp(root string) Area { return Area{Kind: "camp", root: root} }

// Ensure creates the area's own directory with a mode of its own, and
// puts it back if it drifted.
//
// The state directory is 0700 and its records are 0600: a record names
// every path of a composition, and that is nobody else's business.
func (a Area) Ensure(mode os.FileMode) error {
	if a.root == "" {
		return fmt.Errorf("an empty %s area was used", a.Kind)
	}
	if err := os.MkdirAll(a.root, mode); err != nil {
		return fmt.Errorf("creating %s: %w", a.root, err)
	}
	if err := os.Chmod(a.root, mode); err != nil {
		return fmt.Errorf("setting the mode of %s: %w", a.root, err)
	}
	return nil
}

// RemoveSelf removes the area's own directory once it is empty.
func (a Area) RemoveSelf() error {
	if a.root == "" {
		return fmt.Errorf("an empty %s area was used", a.Kind)
	}
	if err := os.Remove(a.root); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", a.root, err)
	}
	return nil
}

// Root returns the area's own directory.
func (a Area) Root() string { return a.root }

// Path resolves a relative path inside the area, refusing anything that
// would climb out of it.
func (a Area) Path(parts ...string) (string, error) {
	if a.root == "" {
		return "", fmt.Errorf("an empty %s area was used", a.Kind)
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.Contains(part, "/") {
			return "", fmt.Errorf("%w: %q is not a single path component", ErrOutside, part)
		}
	}
	return filepath.Join(append([]string{a.root}, parts...)...), nil
}

// Sub returns the area rooted at a subdirectory of this one, so that a
// caller working under work/gen cannot reach the rest of work.
func (a Area) Sub(parts ...string) (Area, error) {
	path, err := a.Path(parts...)
	if err != nil {
		return Area{}, err
	}
	return Area{Kind: a.Kind, root: path}, nil
}

// MkdirAll creates a directory inside the area, and every directory above
// it up to the area's own root.
func (a Area) MkdirAll(parts ...string) (string, error) {
	path, err := a.Path(parts...)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", path, err)
	}
	return path, nil
}

// MkdirAllMode creates a directory with a mode of its own -- the
// privileged mode's staging root is 0700, because until the move it is
// the only place the half-built composition exists.
func (a Area) MkdirAllMode(mode os.FileMode, parts ...string) (string, error) {
	path, err := a.MkdirAll(parts...)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(path, mode); err != nil {
		return "", fmt.Errorf("setting the mode of %s: %w", path, err)
	}
	return path, nil
}

// MkdirDeep creates a directory named by a whole relative path, one
// component at a time, so a mirrored target path can be reproduced inside
// storage.
func (a Area) MkdirDeep(components []string) (string, error) {
	area := a
	for index, component := range components {
		if index == len(components)-1 {
			return area.MkdirAll(component)
		}
		next, err := area.MkdirAll(component)
		if err != nil {
			return "", err
		}
		area = Area{Kind: a.Kind, root: next}
	}
	return a.root, nil
}

// Touch creates an empty regular file if it is not there, and reports
// whether it had to create it.
//
// This is how an attachment point is made for a file island: the
// placeholder lives in camp's own storage, never in a repository.
func (a Area) Touch(parts ...string) (string, bool, error) {
	path, err := a.Path(parts...)
	if err != nil {
		return "", false, err
	}
	if _, err := os.Lstat(path); err == nil {
		return path, false, nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", false, fmt.Errorf("creating %s: %w", path, err)
	}
	file.Close()
	return path, true, nil
}

// Write replaces a file inside the area, atomically.
//
// Written to a temporary file in the same directory and renamed, with
// both file and directory synced, because a record that is half written
// still parses and describes half a composition.
func (a Area) Write(name string, data []byte, mode os.FileMode) error {
	path, err := a.Path(name)
	if err != nil {
		return err
	}
	return WriteAtomic(path, data, mode)
}

// WriteAtomic replaces one file, given a path an Area already vouched
// for.
func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	name := temporary.Name()
	defer os.Remove(name)

	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("setting the mode of %s: %w", path, err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("syncing %s: %w", path, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return syncDir(directory)
}

func syncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("syncing %s: %w", path, err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", path, err)
	}
	return nil
}

// Remove deletes one thing inside the area.
func (a Area) Remove(parts ...string) error {
	path, err := a.Path(parts...)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

// RemoveTree deletes a whole subtree of the area.
//
// The kernel leaves a work/ directory inside the overlay's work
// directory, mode 000 and owned by the invoking user, which cannot be
// walked until it is chmodded -- so this makes every directory it meets
// traversable on the way down. It refuses to touch anything outside the
// area, which is what keeps it from ever being pointed at a repository.
func (a Area) RemoveTree(parts ...string) error {
	path, err := a.Path(parts...)
	if err != nil {
		return err
	}
	return removeTree(path)
}

func removeTree(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("looking at %s: %w", path, err)
	}
	if info.IsDir() {
		// The kernel's leftover is mode 000; without this it cannot even be
		// listed, let alone emptied.
		if info.Mode().Perm()&0o300 != 0o300 {
			if err := os.Chmod(path, 0o700); err != nil {
				return fmt.Errorf("making %s removable: %w", path, err)
			}
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return fmt.Errorf("listing %s: %w", path, err)
		}
		for _, entry := range entries {
			if err := removeTree(filepath.Join(path, entry.Name())); err != nil {
				return err
			}
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", path, err)
	}
	return nil
}

// Chown gives everything in the area to a user.
//
// Used by the privileged helper for the one thing the kernel creates as
// root: the overlay's leftover work directory. The path camp guarantees
// writable must not end up owned by root.
func (a Area) Chown(uid, gid int, parts ...string) error {
	path, err := a.Path(parts...)
	if err != nil {
		return err
	}
	return filepath.WalkDir(path, func(name string, _ os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		return os.Lchown(name, uid, gid)
	})
}
