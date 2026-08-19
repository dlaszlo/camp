package privileged

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/testenv"
)

// sudoSays makes the environment look the way sudo leaves it.
func sudoSays(t *testing.T) {
	t.Helper()
	t.Setenv("SUDO_UID", strconv.Itoa(os.Getuid()))
	t.Setenv("SUDO_GID", strconv.Itoa(os.Getgid()))
}

// The defect this repair is about, at the point where it was decided.
//
// The base was checked by name and then handed on as a name, so every
// later step -- the precheck, each mount's resolution, the reopen after a
// bind, both ends of the move, the teardown -- resolved it again. The
// invoking user owns the environment root and normally its parent, so
// after the check they could rename it away and leave a symlink at the
// old name, and root went on to address this composition's operands
// beneath whatever that link pointed at.
//
// What confine returns is a descriptor. This does the swap and then asks
// the root where it is: it still names the directory it opened, still
// answers with that directory's identity, and a resolution through it
// still reaches the original tree and not the one the link points at.
func TestTheConfinedRootIsNotTheNameItWasOpenedBy(t *testing.T) {
	sudoSays(t)
	scratch := t.TempDir()
	base := filepath.Join(scratch, "env")
	decoy := filepath.Join(scratch, "decoy")
	for _, directory := range []string{base, decoy} {
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "mine"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoy, "theirs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	job := Job{Version: JobVersion, Action: ActionUnmount, Base: base}
	root, err := job.confine()
	if err != nil {
		t.Fatalf("an ordinary base was refused: %v", err)
	}
	defer root.Close()

	if root.Name() != base {
		t.Fatalf("the root names %s and it was opened on %s", root.Name(), base)
	}
	opened, err := root.Identity()
	if err != nil {
		t.Fatal(err)
	}

	// The swap, exactly as the owner of the environment root can perform
	// it: rename it away, leave a link to somewhere else at its name.
	if err := os.Rename(base, filepath.Join(scratch, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(decoy, base); err != nil {
		t.Fatal(err)
	}

	if root.Name() != base {
		t.Errorf("the root's name changed with the directory: %s", root.Name())
	}
	after, err := root.Identity()
	if err != nil {
		t.Fatalf("the root stopped answering after the swap: %v", err)
	}
	if after != opened {
		t.Errorf("the root answers for %+v and it was opened on %+v", after, opened)
	}

	fd, err := root.Open([]string{"mine"}, unix.O_PATH)
	if err != nil {
		t.Errorf("a resolution through the root no longer reaches the tree "+
			"confine opened: %v", err)
	} else {
		unix.Close(fd)
	}
	if fd, err := root.Open([]string{"theirs"}, unix.O_PATH); err == nil {
		unix.Close(fd)
		t.Error("a resolution through the root reached the tree the symlink " +
			"points at, which is the whole defect")
	}

	// And the name itself is refused now rather than followed, so a helper
	// that started after the swap would never open it at all.
	if _, err := job.confine(); err == nil {
		t.Error("a base that is now a symlink was accepted")
	}
}

// The front end and the helper, held together.
//
// Every field the helper insists on is a field the front end fills, and
// nothing keeps the two in step except a test that builds a real job and
// hands it to the real check. Left apart they drift silently: the two
// this test exists for spent a release being written by one half and read
// by neither, while their comments promised checks nobody made.
func TestTheMountJobPassesTheHelpersOwnCheck(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	env := testenv.NewEnv(t)
	built, refused := plan.Prepare(env.Config(t, ""), plan.Privileged)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}
	if err := compose.Directories(built); err != nil {
		t.Fatal(err)
	}
	testenv.Write(t, built.ExcludeFile(), "")

	job, problems := MountJob(built, filepath.Join(built.Work, "staging"), nil)
	if !problems.Empty() {
		t.Fatalf("the mount job was refused:\n%v", problems)
	}
	if err := job.checkable(); err != nil {
		t.Fatalf("the helper refuses a job its own front end built: %v", err)
	}

	var absent, sources int
	for _, operation := range job.Mounts {
		if operation.TargetIdent == "" {
			absent++
			if !operation.TargetAbsent {
				t.Errorf("the mount point %s crosses with neither an identity nor "+
					"a word about why", operation.Target)
			}
		}
		if operation.Kind == string(plan.Overlay) || len(operation.SourceParts) == 0 {
			continue
		}
		sources++
		if operation.SourceIdent == "" {
			t.Errorf("the mount source %s crosses with no identity", operation.Source)
		}
		switch pathx.Type(operation.SourceType) {
		case pathx.Dir, pathx.File:
		default:
			t.Errorf("the mount source %s crosses as %q, which is not a kind "+
				"camp binds", operation.Source, operation.SourceType)
		}
	}
	if absent == 0 {
		t.Error("no mount point in this job is absent, so what the absent flag " +
			"is for was not measured")
	}
	if sources == 0 {
		t.Error("no bind in this job names a source, so what the kind is for " +
			"was not measured")
	}
}
