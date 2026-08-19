package privileged

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/testenv"
)

// The one check that stands between root and a stranger's mount, tested
// where every case can be constructed: a synthetic mount table, because
// making a real mount needs privilege and the point of the check is that
// it happens before any syscall that would need it.
//
// The two dangerous mistakes are opposite ones. Refusing too little means
// root unmounting somebody else's mount because camp wrote that path down
// once. Refusing too much means a teardown that will not finish -- which
// is what happened when this only compared identities: after a teardown
// that unmounted everything and died before saying so, every recorded
// path resolved to what was underneath it, and 'camp down' called eleven
// mounts that were already gone somebody else's.
func TestOnlyAMountThatIsNotCampsIsRefused(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "target")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("this platform does not report a device and inode")
	}
	its := JobTarget{Path: path, Device: uint64(st.Dev), Inode: st.Ino}
	mounted := []mountinfo.Entry{{Point: path}}

	// The identity the check compares is taken from a descriptor resolved
	// beneath the root the helper pinned, from the target's own
	// components, so the root is what the test has to hand it.
	root, err := pathx.OpenRootExactly(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	for _, probe := range []struct {
		name    string
		table   []mountinfo.Entry
		target  JobTarget
		refused bool
	}{
		{
			name:   "nothing is mounted there any more",
			table:  nil,
			target: JobTarget{Path: path, Device: 222, Inode: 6295166},
		},
		{
			name:   "the record carries no identity",
			table:  mounted,
			target: JobTarget{Path: path},
		},
		{
			name:   "the mount is camp's own",
			table:  mounted,
			target: its,
		},
		{
			name:   "the path cannot be looked at",
			table:  []mountinfo.Entry{{Point: filepath.Join(directory, "gone")}},
			target: JobTarget{Path: filepath.Join(directory, "gone"), Device: 1, Inode: 1},
		},
		{
			name:    "something else is mounted there",
			table:   mounted,
			target:  JobTarget{Path: path, Device: 222, Inode: 6295166},
			refused: true,
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			parts, err := componentsBeneath(root, probe.target.Path)
			if err != nil {
				t.Fatal(err)
			}
			mismatch := standsThere(probe.table, root, parts, probe.target)
			if probe.refused && mismatch == "" {
				t.Error("a mount that is not camp's was let through")
			}
			if !probe.refused && mismatch != "" {
				t.Errorf("this must not be refused: %s", mismatch)
			}
			if probe.refused && !strings.Contains(mismatch, probe.target.Path) {
				t.Errorf("the refusal does not name the path: %q", mismatch)
			}
		})
	}
}

// The rename race, at the point where it is decided.
//
// Between the front end's validation and the helper's mount, a component
// the user owns can be replaced -- with a symlink, or with a different
// directory of the same name. The front end's checks would then have been
// about one object and the mount about another. The helper re-resolves
// and compares what it opened against what was checked, and this is that
// comparison: the real race needs a debugger or a slow filesystem to
// construct, the decision it turns on does not.
func TestAnObjectThatIsNotTheOneCampCheckedIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(path, unix.O_PATH, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	its := fmt.Sprintf("%d:%d", st.Dev, st.Ino)

	if err := checkIdentity(fd, path, its); err != nil {
		t.Errorf("the object camp checked was refused: %v", err)
	}
	// What a swapped component looks like from here: the same path, a
	// different object.
	swapped := fmt.Sprintf("%d:%d", st.Dev, st.Ino+1)
	err = checkIdentity(fd, path, swapped)
	if err == nil {
		t.Fatal("an object that is not the one camp checked was accepted")
	}
	if !strings.Contains(err.Error(), "is not the object camp checked") {
		t.Errorf("the refusal should say what happened: %v", err)
	}
	if !strings.Contains(err.Error(), "Nothing has been mounted") {
		t.Errorf("the refusal should say what state the machine is in: %v", err)
	}
	// An operand the front end could not identify carries no expectation,
	// and a comparison against nothing must not invent one.
	if err := checkIdentity(fd, path, ""); err != nil {
		t.Errorf("an operand with no recorded identity was refused: %v", err)
	}
}

// A job whose operands have already moved is refused before the first
// syscall that changes anything, and the reply has to say that the
// machine is clean -- because it is.
//
// The flag is what the front end reads to choose between "nothing is
// mounted" and "what it built is still on the machine: run camp down".
// Measured with a real 'camp up' on 2026-08-18, with the overlay's upper
// layer replaced in the window between the front end's identity capture
// and the helper's first mount: nothing was mounted, the workspace was
// writable, camp status said all eleven mounts were gone -- and camp up
// said the composition was standing and left the record in phase partial.
func TestARefusalBeforeTheFirstMountReportsACleanMachine(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}

	root, err := pathx.OpenRootExactly(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	reply := mount(Job{
		Version:      JobVersion,
		Action:       ActionMount,
		Base:         base,
		StagingParts: []string{"staging"},
		Mounts: []JobMount{{
			Kind:        string(plan.BindRO),
			Target:      target,
			TargetParts: []string{"target"},
			// What the front end saw, and not what is there now.
			TargetIdent: "1:1",
		}},
	}, root)

	if reply.Error == "" {
		t.Fatal("an operand that is not the one camp checked was accepted")
	}
	if !reply.RolledBack {
		t.Error("a refusal that mounted nothing reported a machine still " +
			"carrying a composition")
	}
	if len(reply.Stranded) != 0 {
		t.Errorf("nothing was mounted and this is stranded: %v", reply.Stranded)
	}
	if len(reply.Results) != 0 || reply.Moved {
		t.Errorf("the reply claims work that never happened: %+v", reply)
	}
}

// The self-bind's recording contract, at the one end of it an
// unprivileged test can reach.
//
// Both self-binds the helper makes -- the staging tree's and the live
// directory's -- go on the rollback list on the flag mountx.Detach
// returns and not on the call having succeeded, because the bind and the
// propagation change are two syscalls and the second can fail over a bind
// that took. Here the first syscall is the one that fails: open_tree with
// OPEN_TREE_CLONE needs CAP_SYS_ADMIN. Nothing is attached, so nothing
// may be recorded, and a list that grew here would be a rollback
// unmounting whatever really stands at that path.
//
// The other end -- a bind that exists and a propagation change that
// failed, which is the case the flag exists for -- cannot be reached
// without making a mount, and nothing in this repository may make one.
func TestASelfBindThatWasNotMadeIsNotRecordedForRollback(t *testing.T) {
	testenv.SkipIfItCouldMount(t)

	base := t.TempDir()
	staging := filepath.Join(base, "staging")
	if err := os.Mkdir(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := pathx.OpenRootExactly(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	already := []string{filepath.Join(base, "something-earlier")}
	made, err := detach(root, []string{"staging"}, staging, already)
	if err == nil {
		t.Fatalf("a process without CAP_SYS_ADMIN made a mount. There may now "+
			"be one at %s, and this test is not written to remove it", staging)
	}
	if len(made) != len(already) {
		t.Errorf("a self-bind that was never made was recorded for rollback: %v", made)
	}
	if !strings.Contains(err.Error(), staging) {
		t.Errorf("the refusal does not name the directory: %v", err)
	}

	// And a directory that is not there at all fails before the mount, with
	// the list equally untouched.
	made, err = detach(root, []string{"absent"}, filepath.Join(base, "absent"), already)
	if err == nil {
		t.Fatal("a self-bind onto a directory that does not exist was accepted")
	}
	if len(made) != len(already) {
		t.Errorf("nothing was opened and something was recorded: %v", made)
	}
}

// And the same thing through the helper's own entry point: a staging
// self-bind that could not be made is a refusal that reports a clean
// machine, because the machine is clean.
//
// What the front end does with this reply is choose between "nothing is
// mounted" and "what camp built is still standing: run camp down". A
// rolled-back flag that is wrong in either direction walls somebody in or
// hides a mount from them.
func TestAStagingSelfBindThatFailedLeavesNothingBehind(t *testing.T) {
	testenv.SkipIfItCouldMount(t)

	base := t.TempDir()
	staging := filepath.Join(base, "staging")
	if err := os.Mkdir(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := pathx.OpenRootExactly(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	reply := mount(Job{
		Version:      JobVersion,
		Action:       ActionMount,
		Base:         base,
		StagingParts: []string{"staging"},
	}, root)

	if reply.Error == "" {
		t.Fatalf("a process without CAP_SYS_ADMIN made a mount. There may now "+
			"be one at %s, and this test is not written to remove it", staging)
	}
	if !strings.Contains(reply.Error, staging) {
		t.Errorf("the refusal does not name the staging tree: %s", reply.Error)
	}
	if !reply.RolledBack {
		t.Error("nothing was mounted and the reply says the machine still " +
			"carries a composition")
	}
	if len(reply.Stranded) != 0 {
		t.Errorf("nothing was mounted and this is stranded: %v", reply.Stranded)
	}
	if reply.Moved || len(reply.Results) != 0 {
		t.Errorf("the reply claims work that never happened: %+v", reply)
	}
}
