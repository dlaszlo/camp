package cli_test

import (
	"bytes"
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
