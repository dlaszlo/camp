package fsx_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dlaszlo/camp/internal/fsx"
)

// RemoveTree empties a hostile tree without following any symlink out of
// it and without changing the mode of anything the tree only points at.
//
// The tree it meets in production is the overlay's leftover: directories
// mode 000 that only root can traverse. Making one traversable is the one
// mode change the recursive delete performs, and it is the change the swap
// attack aimed at -- so the tree here mixes the mode-000 shape with the
// links, files and directories a delete must walk past without being
// redirected.
func TestRemoveTreeDoesNotFollowLinksOrChangeOutsideModes(t *testing.T) {
	env := t.TempDir()

	// Outside the area, what a hostile link would reach.
	outside := filepath.Join(env, "outside")
	mkdir(t, filepath.Join(outside, "keep"))
	outsideFile := filepath.Join(outside, "file")
	if err := os.WriteFile(outsideFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	area := fsx.Work(env, "cbfbbb63ee0d")
	if err := area.Ensure(0o755); err != nil {
		t.Fatal(err)
	}
	root, err := area.MkdirAll("victim")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "a-file"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(root, "sub", "deep"))
	if err := os.Symlink(outside, filepath.Join(root, "link-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "link-file")); err != nil {
		t.Fatal(err)
	}
	// The overlay-leftover shape: a directory with no owner write or search,
	// with a child that cannot be reached until the mode is put back.
	locked := filepath.Join(root, "locked")
	mkdir(t, filepath.Join(locked, "inner"))
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}

	if err := area.RemoveTree("victim"); err != nil {
		t.Fatalf("RemoveTree: %v", err)
	}

	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Errorf("the tree was not fully removed: %v", err)
	}
	// The directory a link pointed at, and everything in it, is untouched.
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Errorf("a symlink was followed out of the area: %v", err)
	}
	if info, err := os.Lstat(outsideFile); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("the file a link pointed at changed: %v (err %v)",
			infoMode(info), err)
	}
}

func infoMode(info os.FileInfo) os.FileMode {
	if info == nil {
		return 0
	}
	return info.Mode().Perm()
}
