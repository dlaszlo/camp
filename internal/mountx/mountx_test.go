package mountx_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/mountx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/testenv"
)

// What a test here can and cannot say.
//
// The privileged mount path is open_tree, move_mount, and two attribute
// changes addressed through the created mount's own descriptor. None of
// that can be executed without CAP_SYS_ADMIN, and nothing in this
// repository may mount, so no test below observes a mount being made.
// What they do observe is the refusal: open_tree with OPEN_TREE_CLONE
// fails for an ordinary user, and the shape of what comes back from that
// -- the error, and the separate answer about whether a mount is standing
// -- is the contract every caller's rollback is written against. That
// half is worth holding to on its own, because getting it wrong is a
// rollback acting on a mount that was never made, or leaving one that
// was.

// A self-bind the kernel refuses stands nowhere, and the refusal names
// the directory it was about.
//
// Detach answers two questions, and the helper reads them separately: the
// error says whether it did its job, and the flag says whether a mount is
// on the machine. They differ, because the bind and the propagation
// change are two syscalls and the second one can fail over a bind that
// took. Here the first call is the one that fails, so nothing is
// attached, and the only honest answer to the second question is no -- a
// caller told otherwise would put this directory on its rollback list and
// unmount whatever is really there.
func TestASelfBindTheKernelRefusesStandsNowhere(t *testing.T) {
	testenv.SkipIfItCouldMount(t)

	directory := t.TempDir()
	fd, err := unix.Open(directory, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)

	standing, err := mountx.Detach(fd, directory)
	if err == nil {
		t.Fatalf("a process without CAP_SYS_ADMIN made a mount. There may now "+
			"be one at %s, and this test is not written to remove it", directory)
	}
	if standing {
		t.Error("nothing was attached and the caller was told a mount stands. " +
			"A rollback reading that unmounts whatever is at the path")
	}
	if !strings.Contains(err.Error(), directory) {
		t.Errorf("the refusal does not name the directory it was about: %v", err)
	}
	if !strings.Contains(err.Error(), "can be moved") {
		t.Errorf("the refusal does not say what the self-bind is for: %v", err)
	}
}

// A bind whose detached copy cannot be taken mounts nothing, and says
// which two paths it was about.
//
// The same contract as above, at the operation the helper performs for
// every mount in a plan. The first syscall is the clone; until it and the
// move that follows it have both succeeded there is nothing at the
// target, and the caller has to be told so or it records a mount that
// does not exist.
func TestABindWhoseCopyIsRefusedMountsNothing(t *testing.T) {
	testenv.SkipIfItCouldMount(t)

	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	target := filepath.Join(directory, "target")
	for _, made := range []string{source, target} {
		if err := os.Mkdir(made, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	open := func(path string) int {
		fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { unix.Close(fd) })
		return fd
	}

	placed, err := mountx.MountByDescriptor(
		plan.Mount{Kind: plan.BindRO, Source: source, Target: target},
		open(source), open(target), mountx.NoOperands())
	if err == nil {
		t.Fatalf("a process without CAP_SYS_ADMIN made a mount. There may now "+
			"be one at %s, and this test is not written to remove it", target)
	}
	if placed {
		t.Error("nothing was attached and the caller was told a mount exists " +
			"at the target")
	}
	for _, named := range []string{source, target} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal does not name %s: %v", named, err)
		}
	}
}

// A kind nothing knows is refused before any syscall at all.
//
// Which is why it is safe to hand this one descriptors that cannot be
// used: the refusal happens in the switch, and no operand is touched.
func TestAnUnknownKindTouchesNothing(t *testing.T) {
	placed, err := mountx.MountByDescriptor(
		plan.Mount{Kind: "sideways", Target: "/somewhere"}, -1, -1, mountx.NoOperands())
	if err == nil {
		t.Fatal("a mount kind camp does not know was accepted")
	}
	if placed {
		t.Error("nothing was attempted and the caller was told a mount exists")
	}
	if !strings.Contains(err.Error(), "sideways") {
		t.Errorf("the refusal does not name the kind: %v", err)
	}
}

// The locked flags a remount has to replicate, rendered for the person
// who reads the failure.
//
// A read-only remount inside a user namespace is refused unless it
// carries the source mount's locked flags, so the message that reports
// that refusal has to say which ones camp asked for -- it is the whole
// diagnosis. The strictatime rule is the part worth guarding: a mount
// with no atime option at all is strictatime, and leaving it unsaid reads
// as a request to change it.
func TestTheLockedFlagsAreNamedAsTheMountCarriesThem(t *testing.T) {
	for _, probe := range []struct {
		name    string
		options []string
		want    string
	}{
		{
			name:    "a tmpfs as systemd mounts one",
			options: []string{"rw", "nosuid", "nodev", "relatime"},
			want:    "nosuid,nodev,relatime",
		},
		{
			name:    "nothing about atime, which means strictatime",
			options: []string{"rw", "nosuid"},
			want:    "nosuid,strictatime",
		},
		{
			name:    "an ordinary filesystem with nothing locked",
			options: []string{"rw"},
			want:    "strictatime",
		},
		{
			name:    "an option that is not one of the locked ones",
			options: []string{"rw", "noexec", "someoption"},
			want:    "noexec,strictatime",
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			got := mountx.DescribeFlags(
				mountx.LockedFlags(mountinfo.Entry{Options: probe.options}))
			if got != probe.want {
				t.Errorf("the flags read %q and the mount carries %v; expected %q",
					got, probe.options, probe.want)
			}
		})
	}
	if mountx.DescribeFlags(0) != "none" {
		t.Errorf("an empty flag set has to read as something: %q",
			mountx.DescribeFlags(0))
	}
}
