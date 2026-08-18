package fsx

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// The one place camp changes a mode as root: the kernel leaves an overlay
// work directory mode 000 that only root can clear, and clearing it means
// making it traversable first. Doing that by name is a root primitive to
// chmod anything -- the owner renames the mode-000 directory away and drops
// a symlink at its name between the check and the change, and root follows
// it. So the change acts on the descriptor the type was checked on, never
// on the name.

// chmodFd changes the inode the descriptor holds, not the name it was
// opened from. A symlink swapped in at the name afterwards is changed on
// nothing.
func TestChmodFdActsOnTheDescriptorAndNotTheName(t *testing.T) {
	dir := t.TempDir()

	// The mode-000 directory the cleanup would make traversable.
	victim := filepath.Join(dir, "d")
	if err := os.Mkdir(victim, 0o000); err != nil {
		t.Fatal(err)
	}
	// A file elsewhere. If chmodFd followed the swapped name it would land
	// here and turn a 0600 file into 0700.
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	fd, err := unix.Open(victim, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)

	// The race, arranged deterministically: the directory the descriptor
	// holds is renamed away and its name becomes a link to the file.
	if err := os.Rename(victim, filepath.Join(dir, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, victim); err != nil {
		t.Fatal(err)
	}

	if err := chmodFd(fd, 0o700); err != nil {
		t.Fatalf("chmodFd: %v", err)
	}

	if info, err := os.Lstat(target); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the swapped link's target changed to %v (err %v); chmodFd "+
			"followed the name instead of acting on the descriptor",
			info.Mode().Perm(), err)
	}
	if info, err := os.Stat(filepath.Join(dir, "moved")); err != nil ||
		info.Mode().Perm() != 0o700 {
		t.Errorf("the directory the descriptor held did not get the new mode: "+
			"%v (err %v)", info.Mode().Perm(), err)
	}
}

// The acceptance criterion, kept true by failing the build: no mode change
// in fsx names its object. Fchmodat resolves the name in the call that
// acts, following a symlink swapped in after an earlier check; the change
// has to go through the descriptor the check was made on.
func TestNoModeChangeInFsxNamesItsObject(t *testing.T) {
	data, err := os.ReadFile("fsx.go")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("unix.Fchmodat(")) {
		t.Error("fsx changes a mode by name. A name can be a symlink swapped in " +
			"after the type was checked, so root would follow it. Change the mode " +
			"through the descriptor that was checked, with chmodFd.")
	}
}
