package mountinfo_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dlaszlo/camp/internal/mountinfo"
)

// A table with the shapes that actually turn up: an ordinary mount with
// propagation, a private one with no optional fields, an overlay whose
// super options carry escaped paths, and a mount point containing a
// space.
const table = `25 30 0:23 / /proc rw,nosuid,nodev,noexec,relatime shared:5 - proc proc rw
30 1 252:1 / / rw,relatime - ext4 /dev/sda1 rw,errors=remount-ro
41 30 0:36 / /tmp rw,nosuid,nodev shared:9 - tmpfs tmpfs rw,inode64
88 30 0:52 / /home/x/live rw,relatime - overlay overlay rw,lowerdir=/home/x/ws,upperdir=/home/x/code,workdir=/home/x/.camp/work/abc/work,redirect_dir=nofollow,uuid=on,userxattr
89 30 252:1 /home/x/ws /home/x/ws ro,relatime - ext4 /dev/sda1 rw,errors=remount-ro
90 88 252:1 /home/x/odd /home/x/live/a\040b rw,relatime - ext4 /dev/sda1 rw
91 30 0:52 / /home/y/live rw,relatime - overlay overlay rw,lowerdir=/l,upperdir=/home/x/co\,de,workdir=/w
`

func read(t *testing.T) []mountinfo.Entry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := mountinfo.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestOptionalFieldsDecidePropagation(t *testing.T) {
	entries := read(t)

	root, ok := mountinfo.Top(entries, "/")
	if !ok {
		t.Fatal("/ was not parsed")
	}
	if !root.Private() {
		t.Error("/ has no optional fields here and should read as private")
	}
	proc, _ := mountinfo.Top(entries, "/proc")
	if proc.Private() {
		t.Error("shared:5 is propagation and must not read as private")
	}
}

func TestReadOnlyAndLockedFlagsComeFromThePerMountOptions(t *testing.T) {
	entries := read(t)

	workspace, ok := mountinfo.Top(entries, "/home/x/ws")
	if !ok {
		t.Fatal("the workspace self-bind was not parsed")
	}
	if !workspace.ReadOnly() {
		t.Error("a mount with ro in its options should read as read-only")
	}

	tmp, _ := mountinfo.Top(entries, "/tmp")
	for _, flag := range []string{"nosuid", "nodev"} {
		if !tmp.Has(flag) {
			t.Errorf("/tmp should carry %s; a read-only remount inside a user "+
				"namespace has to replicate it or fail EPERM", flag)
		}
	}
	if tmp.Has("noexec") {
		t.Error("noexec was reported for a mount that does not have it")
	}
}

func TestAMountPointWithASpaceIsDecoded(t *testing.T) {
	entries := read(t)
	if _, ok := mountinfo.Top(entries, "/home/x/live/a b"); !ok {
		t.Error("the escaped space in the mount point was not decoded")
	}
}

// The overlay's super options come back with the kernel's own additions,
// so they have to be compared per option and never as one string.
func TestOverlayOptionsAreSplitPerOption(t *testing.T) {
	entries := read(t)
	overlay, ok := mountinfo.Top(entries, "/home/x/live")
	if !ok {
		t.Fatal("the overlay was not parsed")
	}
	if overlay.FSType != "overlay" {
		t.Errorf("the filesystem type came out as %q", overlay.FSType)
	}
	if got := overlay.Super["upperdir"]; got != "/home/x/code" {
		t.Errorf("upperdir came out as %q", got)
	}
	if got := overlay.Super["lowerdir"]; got != "/home/x/ws" {
		t.Errorf("lowerdir came out as %q", got)
	}
	if _, added := overlay.Super["redirect_dir"]; !added {
		t.Error("the kernel's own added options should be visible, so that " +
			"comparison can ignore them deliberately rather than by accident")
	}
	if _, forced := overlay.Super["userxattr"]; !forced {
		t.Error("userxattr should be visible")
	}
}

// A path containing a comma is escaped inside the option string. Splitting
// naively would tear it in half and then compare two halves against a
// whole.
func TestAnEscapedCommaInAPathSurvives(t *testing.T) {
	entries := read(t)
	overlay, ok := mountinfo.Top(entries, "/home/y/live")
	if !ok {
		t.Fatal("the second overlay was not parsed")
	}
	if got := mountinfo.UnescapeOption(overlay.Super["upperdir"]); got != "/home/x/co,de" {
		t.Errorf("upperdir with a comma came out as %q", got)
	}
}

func TestOverlaysFindsEveryCompositionOnAnUpper(t *testing.T) {
	entries := read(t)
	found := mountinfo.Overlays(entries, "/home/x/code")
	if len(found) != 1 || found[0].Point != "/home/x/live" {
		t.Errorf("the scan for an overlay on /home/x/code found %v", found)
	}
	if len(mountinfo.Overlays(entries, "/home/x/co,de")) != 1 {
		t.Error("the escaped upperdir was not matched")
	}
}

// The privileged mode's own shape, copied from a real run: the live path
// carries a self-bind that gives the move a private parent, and the
// composed tree sits on top of it. The overlay was made first and MS_MOVE
// kept its identity, so it is listed first while standing highest -- the
// parent field is the only thing that says so.
const stacked = `33 1 252:1 / /home rw,relatime - ext4 /dev/sda1 rw
3575 3711 0:228 / /home/x/live rw,relatime - overlay overlay rw,lowerdir=/home/x/ws,upperdir=/home/x/code,workdir=/home/x/.camp/work/abc/work,nouserxattr
3711 33 252:1 /home/x/live /home/x/live rw,relatime - ext4 /dev/sda1 rw
`

func TestTheTopOfAStackIsReadFromTheParentAndNotTheOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(stacked), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := mountinfo.Read(path)
	if err != nil {
		t.Fatal(err)
	}

	top, ok := mountinfo.Top(entries, "/home/x/live")
	if !ok {
		t.Fatal("the live path has no mount")
	}
	if top.FSType != "overlay" {
		t.Fatalf("the live path resolves to a %s mount, and the composed tree "+
			"is the overlay standing on top of the self-bind", top.FSType)
	}
	if got := top.Super["upperdir"]; got != "/home/x/code" {
		t.Errorf("upperdir came out as %q", got)
	}
}

func TestUnderAndContaining(t *testing.T) {
	entries := read(t)

	under := mountinfo.Under(entries, "/home/x/live")
	if len(under) != 2 {
		t.Errorf("%d mounts found under the composed tree, wanted 2", len(under))
	}

	// The filesystem a path sits on is the longest mount point that is a
	// prefix of it -- that is where the locked flags come from.
	on, ok := mountinfo.Containing(entries, "/tmp/scratch/x")
	if !ok || on.Point != "/tmp" {
		t.Errorf("/tmp/scratch/x was placed on %q", on.Point)
	}
	on, ok = mountinfo.Containing(entries, "/var/log")
	if !ok || on.Point != "/" {
		t.Errorf("/var/log was placed on %q", on.Point)
	}
}
