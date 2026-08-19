package mountx

import (
	"os"
	"strings"
	"testing"

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
