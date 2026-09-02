package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/testenv"
)

// A readable table, for the run that has to reach the environment
// section. "/" is in it because that is what the section is derived
// from -- the filesystem each of the environment's paths sits on.
const readableTable = "23 1 0:22 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n"

// A record the kernel cannot have written: the separator is followed by
// two fields and the grammar has three.
const unreadableRecord = "23 1 0:22 / / rw,relatime shared:1 - ext4 /dev/sda1\n"

// aim points doctor at a table of the test's own making.
//
// /proc/self/mountinfo parses on any machine camp can run on at all, so
// the state this is about -- a table camp refuses -- is one nobody can
// arrange on purpose, and doctor's answer to it would otherwise go
// untested until a machine met it.
func aim(t *testing.T, body string) {
	t.Helper()
	path := testenv.Write(t, filepath.Join(t.TempDir(), "mountinfo"), body)
	restore := doctorTable
	doctorTable = path
	t.Cleanup(func() { doctorTable = restore })
}

func doctor(t *testing.T) (string, string, int) {
	t.Helper()
	env := testenv.NewEnv(t)
	env.Config(t, "")

	var out, errOut bytes.Buffer
	code := Main([]string{"doctor", "-f", config.Path(env.Path)}, &out, &errOut)
	return out.String(), errOut.String(), code
}

// One machine, one account of it.
//
// doctor read the table, threw the error away and carried on -- so it
// printed that the configuration is sound and exited 0 on the same host
// state that makes 'camp run', 'camp shell' and 'camp status' refuse.
// Whoever ran doctor to find out why the others were failing was told
// nothing was wrong.
func TestDoctorSaysWhenTheMountTableCannotBeRead(t *testing.T) {
	aim(t, unreadableRecord)
	out, errOut, code := doctor(t)

	if code == ExitOK {
		t.Error("doctor exited 0 on a mount table it could not read")
	}
	if !strings.Contains(errOut, "mount-table-unreadable") {
		t.Errorf("doctor did not name the rule that fired:\n%s", errOut)
	}
	// Not only that it failed: which of doctor's answers are missing, so
	// that nothing else in the report is read as having covered them.
	for _, want := range []string{"did not check", "locked"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("doctor did not say %q -- what was not checked has to be "+
				"named, or the rest of the report reads as complete:\n%s",
				want, errOut)
		}
	}
	// And no verdict about the environment, because every one of them is
	// derived from the table. Half a section under the same heading is an
	// incomplete answer in the shape of a complete one.
	if strings.Contains(out, "this environment:") {
		t.Errorf("doctor reported on the environment from a table it could "+
			"not read:\n%s", out)
	}
}

// The other half of the same rule: with a table it can read, doctor
// reports the environment and says nothing about the mount table at all.
// Without this the test above passes on a doctor that never looks.
func TestDoctorReportsTheEnvironmentFromATableItCanRead(t *testing.T) {
	aim(t, readableTable)
	out, errOut, _ := doctor(t)

	if !strings.Contains(out, "this environment:") {
		t.Errorf("doctor reported nothing about the environment:\n%s", out)
	}
	if strings.Contains(errOut, "mount-table-unreadable") {
		t.Errorf("a table that parses was reported as unreadable:\n%s", errOut)
	}
}
