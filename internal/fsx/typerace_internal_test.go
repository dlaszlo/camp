package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/pathx"
)

// The window these measure is between camp deciding what an object is and
// camp acting on that decision. With the parent directory pinned it is
// the final component that can change underneath, and the two changes
// that matter are a directory becoming a file and a file becoming a
// directory. Absence was folded together with both of them, so a removal
// that met the replacement, and a chown that met it, reported success
// over an object neither of them had touched.
//
// The swap is made in afterTypeCheck, the seam that exists for this: a
// test cannot otherwise be inside that window, and a race arranged with
// goroutines and sleeps would measure the scheduler.

// hostile is an environment root with a work area under it, ready to be
// written into.
func hostile(t *testing.T) (Area, string) {
	t.Helper()
	env := t.TempDir()
	root, err := pathx.OpenRoot(env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	area := Work(root, "cbfbbb63ee0d")
	if err := area.Ensure(0o755); err != nil {
		t.Fatal(err)
	}
	return area, area.Root()
}

// swapOnce arranges for one type change, the first time the walk looks at
// the named object, and puts the seam back when the test ends.
func swapOnce(t *testing.T, want string, change func()) {
	t.Helper()
	done := false
	afterTypeCheck = func(name string) {
		if name != want || done {
			return
		}
		done = true
		change()
	}
	t.Cleanup(func() { afterTypeCheck = func(string) {} })
}

// A directory that becomes a file between the stat and the O_DIRECTORY
// open used to end the removal with success: the open failed with
// ENOTDIR, ENOTDIR counted as absence, and RemoveTree said the name was
// clear with the replacement standing at it.
func TestRemoveTreeDoesNotReportADirectoryGoneWhenAFileTookItsPlace(t *testing.T) {
	area, path := hostile(t)
	victim := filepath.Join(path, "victim")
	if _, err := area.MkdirAll("victim"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "inside"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	swapOnce(t, "victim", func() {
		if err := os.RemoveAll(victim); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(victim, []byte("a replacement"), 0o644); err != nil {
			t.Error(err)
		}
	})

	if err := area.RemoveTree("victim"); err != nil {
		t.Fatalf("RemoveTree: %v", err)
	}
	// Success is allowed only because the name really is clear now: the
	// second pass removed what the first one found in its place.
	if _, err := os.Lstat(victim); !os.IsNotExist(err) {
		t.Errorf("RemoveTree reported the name clear and %s is still there", victim)
	}
}

// The other direction: a file that becomes a directory. The unlink meets
// EISDIR, which was at least not swallowed -- it came back as a failed
// removal. A second pass reaches the answer the caller asked for, which
// is that the name is gone.
func TestRemoveTreeFinishesWhenAFileBecomesADirectory(t *testing.T) {
	area, path := hostile(t)
	victim := filepath.Join(path, "victim")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	swapOnce(t, "victim", func() {
		if err := os.Remove(victim); err != nil {
			t.Error(err)
		}
		if err := os.MkdirAll(filepath.Join(victim, "inside"), 0o755); err != nil {
			t.Error(err)
		}
	})

	if err := area.RemoveTree("victim"); err != nil {
		t.Fatalf("RemoveTree: %v", err)
	}
	if _, err := os.Lstat(victim); !os.IsNotExist(err) {
		t.Errorf("RemoveTree reported the name clear and %s is still there", victim)
	}
}

// A name that never settles is refused rather than retried for ever. The
// swap here is made on every pass, so no pass can reach a stable answer,
// and what comes back names the race instead of claiming a removal.
func TestRemoveTreeRefusesANameThatKeepsChangingType(t *testing.T) {
	area, path := hostile(t)
	victim := filepath.Join(path, "victim")
	if _, err := area.MkdirAll("victim"); err != nil {
		t.Fatal(err)
	}

	// Whatever the pass just decided the name is, it is the other thing by
	// the time the pass acts on it.
	afterTypeCheck = func(name string) {
		if name != "victim" {
			return
		}
		info, err := os.Lstat(victim)
		if err != nil {
			return
		}
		if info.IsDir() {
			os.RemoveAll(victim)
			os.WriteFile(victim, []byte("a replacement"), 0o644)
			return
		}
		os.Remove(victim)
		os.MkdirAll(victim, 0o755)
	}
	t.Cleanup(func() { afterTypeCheck = func(string) {} })

	err := area.RemoveTree("victim")
	if !errors.Is(err, ErrChangedType) {
		t.Fatalf("RemoveTree returned %v, wanted a refusal naming the race", err)
	}
}

// Chown refuses instead of retrying, because it is asked to give a known
// subtree to a user and a replacement is not that subtree. It used to
// report the whole tree given away without descending into any of it.
func TestChownRefusesADirectoryReplacedByAFile(t *testing.T) {
	area, path := hostile(t)
	tree := filepath.Join(path, "tree")
	if _, err := area.MkdirAll("tree"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "inside"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	swapOnce(t, "tree", func() {
		if err := os.RemoveAll(tree); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(tree, []byte("a replacement"), 0o644); err != nil {
			t.Error(err)
		}
	})

	// The invoking user's own ids: an unprivileged process may give a file
	// to the user who already owns it, which is enough to reach the walk.
	err := area.Chown(os.Getuid(), os.Getgid(), "tree")
	if !errors.Is(err, ErrChangedType) {
		t.Fatalf("Chown returned %v, wanted a refusal naming the race", err)
	}
}

// The same fold, without a race: a file standing where an area's own
// directory should be. AT_REMOVEDIR answers ENOTDIR, which was read as
// absence, so the removal reported a directory gone that had never been a
// directory and is still there.
func TestRemovingAnAreaWhoseNameIsAFileIsNotARemoval(t *testing.T) {
	env := t.TempDir()
	root, err := pathx.OpenRoot(env)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	area := Work(root, "cbfbbb63ee0d")
	if err := os.MkdirAll(filepath.Dir(area.Root()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(area.Root(), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = area.RemoveSelf()
	if !errors.Is(err, unix.ENOTDIR) {
		t.Fatalf("RemoveSelf returned %v, wanted the kernel's type error", err)
	}
	if _, err := os.Lstat(area.Root()); err != nil {
		t.Errorf("%s went away after all: %v", area.Root(), err)
	}
}
