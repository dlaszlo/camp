package mountx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/mountinfo"
)

// Which mounts have a descriptor route and which do not, decided where
// the kernel decides it: on propagation.
//
// This is the rule the whole graveyard turns on. A mount that is shared,
// or whose parent is shared, cannot be moved anywhere -- so it cannot be
// taken somewhere only root can name, and it comes down by a name the
// kernel resolves again. camp's two self-binds are exactly that case on
// any systemd machine, and everything the composition itself mounts is
// exactly the other one, because the staging and live points were made
// private before anything was built in them.
//
// The table here is synthetic, which is the point: the propagation
// arrangement it describes is the one a real 'camp up' produces, and no
// privilege is needed to hold the decision to it.
func TestOnlyAMountNothingSharesCanBeTakenToTheGraveyard(t *testing.T) {
	// / is shared, as it is on every systemd machine. The staging point is
	// bound onto itself and made private, so the mount inside it is
	// private and has a private parent -- and the self-bind itself is
	// private with a shared parent.
	table := []mountinfo.Entry{
		{ID: 1, Parent: 0, Point: "/", Optional: []string{"shared:1"}},
		{ID: 20, Parent: 1, Point: "/env/.camp/work/h/staging"},
		{ID: 21, Parent: 20, Point: "/env/.camp/work/h/staging/code"},
		{ID: 30, Parent: 1, Point: "/env/live", Optional: []string{"shared:9"}},
	}

	for _, probe := range []struct {
		name   string
		path   string
		can    bool
		reason string
	}{
		{
			name:   "a mount camp made inside its private staging tree",
			path:   "/env/.camp/work/h/staging/code",
			can:    true,
			reason: "the composition's own mounts are what the graveyard is for",
		},
		{
			name:   "the self-bind that gave it that private parent",
			path:   "/env/.camp/work/h/staging",
			can:    false,
			reason: "its parent is /, which is shared, and the kernel refuses the move",
		},
		{
			name:   "a mount that is itself shared",
			path:   "/env/live",
			can:    false,
			reason: "the kernel refuses to move a shared mount",
		},
		{
			name:   "a path the table does not name",
			path:   "/env/.camp/work/h/staging/gone",
			can:    true,
			reason: "not knowing is not evidence, and move_mount answers for itself",
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			if got := movable(table, probe.path); got != probe.can {
				t.Errorf("movable(%s) is %v and should be %v: %s",
					probe.path, got, probe.can, probe.reason)
			}
		})
	}
}

// A graveyard nobody needed is a graveyard nobody made.
//
// Every unmount in the privileged helper goes through one of these, and
// most invocations remove nothing at all -- a job refused before its
// first syscall, a teardown whose every recorded place is already clear.
// Making the directory and its mount for those would put a mount on the
// machine for no mount removed, so nothing happens until the first one
// has to come down, and closing an unused one touches nothing.
func TestAGraveyardNothingAskedForIsNeverMade(t *testing.T) {
	if err := NewGraveyard().Close(); err != nil {
		t.Errorf("closing a graveyard that was never opened: %v", err)
	}
}

// An ordinary user cannot make one, and that is the shape of the
// protection rather than an inconvenience.
//
// The graveyard is in /run because /run belongs to root: no part of the
// path can be renamed by the person the helper is defending against, which
// is what makes the unmount at the end of it safe. The same fact means
// this test's own process cannot create it, and the refusal has to name
// the directory, because the person reading it needs to know which one
// root could not write in.
func TestOnlyRootCanMakeTheGraveyard(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this measures what an ordinary user is refused")
	}
	grave := NewGraveyard()
	defer grave.Close()

	err := grave.Open()
	if err == nil {
		t.Fatalf("an ordinary user made %s/camp/graveyard", GraveyardBase)
	}
	if !strings.Contains(err.Error(), GraveyardBase) {
		t.Errorf("the refusal does not name the directory: %v", err)
	}

	// And it is asked once. The answer is a fact about the machine, and a
	// teardown with twenty targets must not try twenty times.
	if second := grave.Open(); second.Error() != err.Error() {
		t.Errorf("the second answer differs from the first:\n%v\n%v", err, second)
	}
}

// The descriptor the caller decided on is closed before anything is
// unmounted, and Remove says so.
//
// This is the rule the whole route turns on and the one it is easiest to
// lose. A descriptor on a mount is a reference to it and umount2 answers
// EBUSY while any is held -- C35 measured that as the reason the obvious
// repair cannot work, and it is the same reference whoever holds it. Remove
// closes its own handles before the call; the caller's descriptor is the
// one it cannot close by agreement, because the caller would then close a
// number the kernel had since given to something else. So it closes it and
// leaves -1 behind.
//
// Written after the mistake: the first version of this left the caller's
// descriptor open across the unmount, and every mount the helper tried to
// remove came back busy -- the composition it had just built, at its own
// path, held by camp.
func TestRemoveClosesTheDescriptorItDecidedOn(t *testing.T) {
	// A plain directory, not a mount point. What the removal answers is not
	// what this measures: it is that whichever way out Remove takes, the
	// descriptor is gone by the end of it.
	base := t.TempDir()
	fd, err := unix.Open(base, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	held := Mounted{FD: fd, Dir: -1, Name: filepath.Base(base), Path: base}
	defer held.Close()

	if _, err := NewGraveyard().Remove(&held); err != nil && held.FD >= 0 {
		t.Fatalf("Remove failed with the descriptor still open: %v", err)
	}
	if held.FD != -1 {
		t.Fatalf("the descriptor is still %d, and holding it across an "+
			"unmount is what makes the unmount fail", held.FD)
	}
	// And the number itself is deliberately not looked at again. A closed
	// descriptor's number is free, so by now it may belong to something
	// else this process opened -- which is the whole reason Remove closes
	// it and reports that it did, instead of leaving the caller to close a
	// number twice.
}

// The rule UnmountIn rests on, held against the swap it exists to survive.
//
// umount2 takes a path and nothing else, so the only way to name a mount
// to it by descriptor is the descriptor's own /proc/self/fd name with one
// component appended. What that has to mean, and what this measures: the
// kernel resolves the magic link to the directory the descriptor holds --
// not by walking the name it was opened by -- and then walks the one
// component from there. So renaming that directory away and leaving a
// link to somewhere else at its name changes nothing about where this
// arrives.
//
// No mount and no privilege: the addressing is the half that decides, and
// stat answers the same question umount2 would ask. That the call then
// acts on the mount standing at what it reached is the other half, and the
// rename race measures it end to end.
func TestADescriptorsOwnNameIsNotRedirectedByRenamingIt(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	decoy := filepath.Join(base, "decoy")
	for _, directory := range []string{real, decoy} {
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "child"),
			[]byte(filepath.Base(directory)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dir, err := unix.Open(real, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(dir)

	// The swap the invoking user can make at any instant: the directory
	// goes somewhere else and a link to another tree takes its name.
	if err := os.Rename(real, filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(decoy, real); err != nil {
		t.Fatal(err)
	}

	through := fmt.Sprintf("/proc/self/fd/%d/child", dir)
	reached, err := os.ReadFile(through)
	if err != nil {
		t.Fatalf("reading %s: %v", through, err)
	}
	if string(reached) != "real" {
		t.Errorf("%s reached the %s tree. A rename above the descriptor "+
			"redirected it, and the unmount camp performs this way would be "+
			"root acting wherever the link pointed", through, reached)
	}

	// And by the name it was opened under, which is what camp used to hand
	// the kernel: straight into the decoy.
	if byName, err := os.ReadFile(filepath.Join(real, "child")); err != nil {
		t.Fatal(err)
	} else if string(byName) != "decoy" {
		t.Fatalf("the swap did not take: %s reads %q", real, byName)
	}
}
