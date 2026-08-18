package fsx_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/pathx"
)

// The invariant this file guards: camp writes inside the places it owns
// and nowhere else, whatever anybody has left lying in the path.
//
// The interesting case is a symlink. A repository is a directory the user
// can write, and camp's own directory sits beside it -- so a link at
// $ENV/.camp/work pointing into the code repository is something anybody
// can plant, by hand or by a program that thinks it is being helpful.
// Joining strings and calling MkdirAll would follow it, and camp would
// create, chown and remove things inside a repository while every
// source-level guard stayed green.

// A symlink standing where one of the area's own components should be is
// refused, and the directory it points at is left untouched.
func TestASymlinkInTheAreasOwnPathIsRefused(t *testing.T) {
	env := t.TempDir()
	repository := filepath.Join(env, "code")
	mkdir(t, filepath.Join(repository, "src"))
	mkdir(t, filepath.Join(env, ".camp"))

	// .camp/work is a link into the repository. Everything below it now
	// resolves inside somebody's source tree.
	if err := os.Symlink(repository, filepath.Join(env, ".camp", "work")); err != nil {
		t.Fatal(err)
	}

	area := fsx.Work(environment(t, env), "cbfbbb63ee0d")
	err := area.Ensure(0o755)
	if err == nil {
		t.Fatal("the area was created through a symlink into a repository")
	}
	if !errors.Is(err, fsx.ErrOutside) {
		t.Errorf("the refusal does not say the path leaves the area: %v", err)
	}
	if entries, _ := os.ReadDir(repository); len(entries) != 1 {
		t.Errorf("the repository was written into: %v", entries)
	}
}

// The same, one level further down: the area exists and something inside
// it is a link out. Every operation is addressed by component from the
// base, so every one of them refuses.
func TestNothingIsWrittenThroughASymlinkInsideAnArea(t *testing.T) {
	env := t.TempDir()
	outside := filepath.Join(env, "elsewhere")
	mkdir(t, outside)

	area := fsx.Work(environment(t, env), "cbfbbb63ee0d")
	if err := area.Ensure(0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(area.Root(), "gen")); err != nil {
		t.Fatal(err)
	}

	if _, err := area.MkdirAll("gen", "out"); err == nil {
		t.Error("a directory was created through the link")
	}
	if _, err := os.Stat(filepath.Join(outside, "out")); err == nil {
		t.Error("the directory landed outside the area")
	}

	// A file written under that name replaces the link rather than
	// following it: renaming onto a symlink replaces the link itself. camp
	// owns this directory, so its own file standing where somebody put a
	// link is the right outcome -- and nothing outside was touched.
	if err := area.Write("gen", []byte("x"), 0o644); err != nil {
		t.Errorf("writing camp's own file over the link failed: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(area.Root(), "gen")); err != nil ||
		info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("the link is still there: %v %v", info, err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("something was written where the link pointed: %v", entries)
	}
}

// A path that climbs out of the area is refused before any syscall, so
// the caller gets a message about its own mistake.
func TestAComponentThatClimbsOutIsRefused(t *testing.T) {
	env := t.TempDir()
	area := fsx.Work(environment(t, env), "cbfbbb63ee0d")
	if err := area.Ensure(0o755); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"..", ".", "", "a/b", "a\x00b"} {
		if _, err := area.MkdirAll(part); !errors.Is(err, fsx.ErrOutside) {
			t.Errorf("%q was accepted as a component: %v", part, err)
		}
	}
}

// The swap a check-then-write cannot survive: the name is a directory
// when it is looked at and a link when it is written through. There is no
// gap here to swap in -- the kernel resolves the component in the call
// that acts on it -- so the write lands where the area says or nowhere.
func TestASwapAfterTheLookIsStillRefused(t *testing.T) {
	env := t.TempDir()
	outside := filepath.Join(env, "elsewhere")
	mkdir(t, outside)

	area := fsx.Work(environment(t, env), "cbfbbb63ee0d")
	if err := area.Ensure(0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := area.MkdirAll("gen"); err != nil {
		t.Fatal(err)
	}
	// Everything camp knows about the area is true at this moment. Now the
	// directory becomes a link, which is what an attacker with a shell in
	// the user's own account has all the time in the world to arrange.
	if err := os.Remove(filepath.Join(area.Root(), "gen")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(area.Root(), "gen")); err != nil {
		t.Fatal(err)
	}

	if _, _, err := area.Touch("gen", "island"); err == nil {
		t.Error("a file was created through the swapped link")
	}
	if _, err := os.Stat(filepath.Join(outside, "island")); err == nil {
		t.Error("the file landed outside the area")
	}
}

// State is the one area whose base camp did not make: the user's own
// state directory, which XDG_STATE_HOME may point anywhere -- including
// into a repository. camp's own directory below it is still resolved the
// strict way, so a link there is refused like any other.
func TestTheStateAreaIsConfinedTheSameWay(t *testing.T) {
	base := t.TempDir()
	repository := filepath.Join(base, "code")
	mkdir(t, repository)
	if err := os.Symlink(repository, filepath.Join(base, "camp")); err != nil {
		t.Fatal(err)
	}

	if err := (fsx.State(environment(t, base), "camp")).Ensure(0o700); err == nil {
		t.Fatal("records would be written into a repository through a link")
	}
	if entries, _ := os.ReadDir(repository); len(entries) != 0 {
		t.Errorf("the repository was written into: %v", entries)
	}
}

// environment opens a directory the way a parsed configuration opens the
// environment root: once, held for as long as anything addresses areas
// from it.
func environment(t *testing.T, path string) pathx.Root {
	t.Helper()
	root, err := pathx.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
