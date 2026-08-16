package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/cli"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/testenv"
)

// run invokes a command the way a terminal does, and returns what each
// stream received.
func run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Main(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// refusing builds an environment whose composition cannot start: the same
// name at both repository roots, which the gate refuses. The plan itself
// still derives, which is exactly the situation this test is about.
func refusing(t *testing.T) string {
	t.Helper()
	env := testenv.NewEnv(t)
	testenv.Write(t, filepath.Join(env.Code, "AGENTS.md"), "the code's own\n")
	env.Config(t, "")
	return config.Path(env.Path)
}

// explain describes a tree to whoever is standing in it. Rendered beside a
// standing refusal it would describe a tree that will not exist, and a
// description reads as authority in a way a plan does not -- so every
// refusal stops it, not only the ones that left no plan behind.
func TestExplainDescribesNothingWhileARefusalStands(t *testing.T) {
	path := refusing(t)

	for _, mode := range [][]string{
		{"explain", "-f", path},
		{"explain", "--privileged", "-f", path},
	} {
		out, errOut, code := run(t, mode...)
		if code == 0 {
			t.Errorf("%v exited 0 with a refusal standing", mode)
		}
		if strings.Contains(out, "You are in") {
			t.Errorf("%v described the tree anyway:\n%s", mode, out)
		}
		if !strings.Contains(errOut, "AGENTS.md") {
			t.Errorf("%v did not name what stops the composition:\n%s", mode, errOut)
		}
	}
}

// A configuration that no longer parses must not stand between the user
// and a teardown.
//
// down tears down from its record and reads the file only to learn which
// record. A file edited while the composition was up -- and the session:
// section adds a whole class of ways to get that wrong -- would otherwise
// leave somebody behind mounts camp made and now refuses to remove, which
// is the one thing down is never allowed to do.
func TestATeardownIsNotBlockedByAConfigurationThatWillNotParse(t *testing.T) {
	env := testenv.NewEnv(t)
	env.Config(t, "")
	path := config.Path(env.Path)

	broken, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Everything that names the tree still parses; the session section does
	// not.
	testenv.Write(t, path, string(broken)+"\nsession:\n  environment:\n    PORT: 8080\n")

	out, errOut, code := run(t, "down", "-f", path)

	// There is no record here, so the teardown ends by saying so -- which is
	// exactly the point: it got past the configuration to the record, and
	// stopped on the record's own terms.
	if !strings.Contains(errOut, "no record for") {
		t.Errorf("down stopped on the configuration instead of reaching the "+
			"record:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !strings.Contains(errOut, "environment-shape") && !strings.Contains(errOut, "PORT") {
		t.Errorf("down did not say what it could not read in the file:\n%s", errOut)
	}
	if !strings.Contains(errOut, "goes ahead anyway") {
		t.Errorf("down did not say that the teardown is unaffected:\n%s", errOut)
	}
	if code == 0 {
		t.Error("down exited 0 with nothing to tear down")
	}
}

// The same file stops every command that genuinely needs to understand it.
// Tolerating a broken configuration is a property of the teardown alone,
// not a general loosening.
func TestABrokenSectionStillStopsTheCommandsThatNeedIt(t *testing.T) {
	env := testenv.NewEnv(t)
	env.Config(t, "")
	path := config.Path(env.Path)

	broken, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	testenv.Write(t, path, string(broken)+"\nsession:\n  environment:\n    PORT: 8080\n")

	for _, command := range [][]string{
		{"plan", "-f", path},
		{"explain", "-f", path},
		{"status", "-f", path},
	} {
		_, errOut, code := run(t, command...)
		if code == 0 {
			t.Errorf("%v accepted a configuration it cannot read", command)
		}
		if !strings.Contains(errOut, "PORT") {
			t.Errorf("%v did not name the entry it could not read:\n%s", command, errOut)
		}
	}
}

// plan is the other half of the same rule, and it behaves differently on
// purpose: it prints the derived plan and then what stops it, because the
// plan is what somebody repairing the configuration needs to look at.
func TestPlanStillPrintsThePlanBesideTheRefusal(t *testing.T) {
	path := refusing(t)

	out, _, code := run(t, "plan", "-f", path)
	if code == 0 {
		t.Error("plan exited 0 with a refusal standing")
	}
	if !strings.Contains(out, "mount sequence, in order:") {
		t.Errorf("plan printed no sequence:\n%s", out)
	}
	if !strings.Contains(out, "would not start") {
		t.Errorf("plan did not say the composition would not start:\n%s", out)
	}
}
