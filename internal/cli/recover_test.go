package cli_test

import (
	"os"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/state"
	"github.com/dlaszlo/camp/internal/testenv"
)

// recorded builds an environment, writes the record a privileged up would
// have written, and then deletes the configuration -- the situation the
// kill-point matrix (spec §22) puts camp in deliberately.
func recorded(t *testing.T) (state.Record, *testenv.Env) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	built, refused := plan.Prepare(cfg, plan.Privileged)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}

	record := state.FromPlan(built, "test", "", "", os.Getuid(), os.Getgid())
	record.Phase = state.Up
	if err := record.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.Source); err != nil {
		t.Fatal(err)
	}
	return record, env
}

// status describes a composition with no configuration anywhere in reach.
//
// Measured on a real privileged composition before this existed: with the
// file moved aside, 'camp status' and 'camp down' both answered "no
// .camp/config.yml here or in any parent directory" while the workspace
// was read-only for the whole machine and the record held the entire
// plan. The record is what recovery stands on (§12), so all three ways of
// naming a composition have to reach it without the file.
func TestStatusDescribesACompositionWithTheConfigurationDeleted(t *testing.T) {
	record, env := recorded(t)

	byRecord := func(t *testing.T, out string) {
		t.Helper()
		if !strings.Contains(out, record.Live) {
			t.Errorf("status did not name the composed tree:\n%s", out)
		}
		if !strings.Contains(out, "phase:") || !strings.Contains(out, string(state.Up)) {
			t.Errorf("status did not say what phase the record is in:\n%s", out)
		}
		if !strings.Contains(out, "recorded mount(s)") {
			t.Errorf("status did not list the recorded mounts:\n%s", out)
		}
		// Nothing is mounted in this test, and the record says up. Saying so
		// is the answer -- and it is the answer the old status could not give
		// at all.
		if !strings.Contains(out, "nothing the record names is mounted") {
			t.Errorf("status did not say what is on the machine:\n%s", out)
		}
		// And a record that says one thing while the machine says another is
		// named as exactly that, rather than reported as a healthy tree.
		if !strings.Contains(out, "record says 'up' and nothing of it is mounted") {
			t.Errorf("status did not name the disagreement:\n%s", out)
		}
	}

	// The exit is non-zero throughout: the record says up and nothing of it
	// is mounted, which is a state somebody has to resolve. What is being
	// measured here is that all three ways of naming the composition reach
	// the same description without a configuration.
	out, errOut, code := run(t, "status", "-record", record.Hash)
	if code == 0 {
		t.Errorf("status by record exited 0 over a record the machine "+
			"contradicts:\n%s", errOut)
	}
	byRecord(t, out)

	out, _, _ = run(t, "status", "-live", env.Live)
	byRecord(t, out)

	// And standing in the environment, with nothing named at all: the
	// record says which directory it belongs to, so the directory can find
	// the record.
	t.Chdir(env.Path)
	out, _, _ = run(t, "status")
	byRecord(t, out)
}

// down reaches the same record, and says the same thing when there is
// none. It is the command somebody runs when they are walled in behind
// mounts, so "no configuration" must never be its answer.
func TestDownFindsTheRecordWithTheConfigurationDeleted(t *testing.T) {
	record, env := recorded(t)

	// Forget it, so that the run stops before the helper: what is being
	// tested here is which composition down selects, not the unmounting,
	// which needs a terminal and a real machine.
	if err := state.Forget(record.Hash); err != nil {
		t.Fatal(err)
	}

	t.Chdir(env.Path)
	_, errOut, code := run(t, "down")
	if code == 0 {
		t.Error("down exited 0 with no record to act on")
	}
	if strings.Contains(errOut, "config.yml") {
		t.Errorf("down answered with the missing configuration instead of the "+
			"missing record:\n%s", errOut)
	}
	if !strings.Contains(errOut, "no record for") {
		t.Errorf("down did not say that there is no record:\n%s", errOut)
	}
}

// Two compositions can claim one directory -- an environment nested
// inside another's tree. camp names them and stops rather than choosing.
func TestATiedDirectoryIsRefusedRatherThanGuessedAt(t *testing.T) {
	first, env := recorded(t)

	second := first
	second.Hash = "0000cafe0000"
	second.Live = env.Live + "-other"
	if err := second.Save(); err != nil {
		t.Fatal(err)
	}

	t.Chdir(env.Path)
	_, errOut, code := run(t, "status")
	if code == 0 {
		t.Error("status chose between two compositions instead of stopping")
	}
	if !strings.Contains(errOut, first.Hash) || !strings.Contains(errOut, second.Hash) {
		t.Errorf("the refusal did not name both compositions:\n%s", errOut)
	}
	if !strings.Contains(errOut, "-record") {
		t.Errorf("the refusal did not say how to name one:\n%s", errOut)
	}
}

// explain has two sources, and which one it uses is decided by what is
// mounted rather than by which file exists.
//
// A record left by a composition that is no longer there describes a tree
// nobody is standing in. The configuration is then the only honest
// source -- and in the namespace mode, which leaves no record at all, it
// is the only source there is.
func TestExplainDoesNotDescribeARecordedTreeThatIsNotThere(t *testing.T) {
	record, env := recorded(t)

	// Put the configuration back: with nothing mounted, that is what
	// explain has to fall back to.
	cfg := env.Config(t, "")
	if cfg.Live() != record.Live {
		t.Fatalf("the fixture moved: %s vs %s", cfg.Live(), record.Live)
	}

	t.Chdir(env.Path)
	out, errOut, code := run(t, "explain")
	if code != 0 {
		t.Fatalf("explain exited %d:\n%s", code, errOut)
	}
	if !strings.Contains(out, "You are in "+record.Live) {
		t.Errorf("explain did not describe the tree:\n%s", out)
	}
	// The record is privileged and nothing is mounted; the description has
	// to be the one the configuration derives, which is the namespace mode.
	if strings.Contains(out, "read-only for all of them") {
		t.Errorf("explain described a machine-wide composition that is not "+
			"there:\n%s", out)
	}
}
