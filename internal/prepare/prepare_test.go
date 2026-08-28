package prepare_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/prepare"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/testenv"
)

// The prepare commands are the environment's own programs, run before
// anything of the composition exists. Every test here runs real
// processes: what is being measured is what camp starts and what it does
// with how they ended, and a fake process would measure neither.

// with writes a configuration carrying the given prepare: block.
func with(t *testing.T, env *testenv.Env, block string) config.Config {
	t.Helper()
	return env.Config(t, env.YAML()+"\nprepare:\n"+block)
}

func mustRefuse(t *testing.T, refused refusal.List, rule string) string {
	t.Helper()
	if refused.Empty() {
		t.Fatalf("expected the rule %q to fire, and nothing was refused", rule)
	}
	found := refused.Rules()
	for _, name := range found {
		if name == rule {
			return refused.Error()
		}
	}
	t.Fatalf("expected the rule %q to fire; the rules that did were %v\n\n%v",
		rule, found, refused.Error())
	return ""
}

// The list is ordered, and the order is the order they run in.
func TestThePrepareCommandsRunInOrder(t *testing.T) {
	env := testenv.NewEnv(t)
	trail := filepath.Join(env.Path, "trail")
	cfg := with(t, env, ""+
		"  - command: [\"/bin/sh\", \"-c\", \"echo first >> "+trail+"\"]\n"+
		"  - command: [\"/bin/sh\", \"-c\", \"echo second >> "+trail+"\"]\n"+
		"  - command: [\"/bin/sh\", \"-c\", \"echo third >> "+trail+"\"]\n")

	if refused := prepare.Run(cfg); !refused.Empty() {
		t.Fatalf("three commands that all succeed were refused:\n%v", refused.Error())
	}
	if got := read(t, trail); got != "first\nsecond\nthird\n" {
		t.Errorf("the commands ran as %q", got)
	}
}

// The first one that fails refuses the composition, and the ones after it
// do not run. These are programs that change things: carrying on past one
// that said stop is the guessing camp does not do.
func TestTheFirstFailureStopsTheOnesAfterIt(t *testing.T) {
	env := testenv.NewEnv(t)
	trail := filepath.Join(env.Path, "trail")
	cfg := with(t, env, ""+
		"  - command: [\"/bin/sh\", \"-c\", \"echo first >> "+trail+"\"]\n"+
		"  - command: [\"/bin/sh\", \"-c\", \"exit 3\"]\n"+
		"  - command: [\"/bin/sh\", \"-c\", \"echo third >> "+trail+"\"]\n")

	message := mustRefuse(t, prepare.Run(cfg), "prepare-failed")
	if got := read(t, trail); got != "first\n" {
		t.Errorf("the trail is %q: the command after the failing one ran", got)
	}
	if !strings.Contains(message, "prepare command 2") {
		t.Errorf("the refusal has to say which command failed:\n%s", message)
	}
	// The part a generation step's message does not have to carry. A
	// generator writes into camp's scratch, so a failed one leaves the
	// machine as it was; these are the environment's own programs and may
	// have changed a repository before stopping.
	if !strings.Contains(message, "still changed") {
		t.Errorf("the refusal has to say that what the command changed before "+
			"it stopped is still changed, rather than leaving 'nothing has been "+
			"mounted' to read as 'nothing happened':\n%s", message)
	}
}

// Where a command stands when it runs, and the two paths it is handed.
// The environment root and not camp's scratch: the scratch is named after
// a plan that does not exist at this point, and every path a prepare
// command cares about is under the environment root.
func TestACommandStandsInTheEnvironmentRootAndIsHandedTheTwoPaths(t *testing.T) {
	env := testenv.NewEnv(t)
	answer := filepath.Join(env.Path, "answer")
	cfg := with(t, env, "  - command: [\"/bin/sh\", \"-c\", "+
		"\"{ pwd; echo $CAMP_ENV; echo $CAMP_LIVE; } > "+answer+"\"]\n")

	if refused := prepare.Run(cfg); !refused.Empty() {
		t.Fatalf("the command was refused:\n%v", refused.Error())
	}
	want := env.Path + "\n" + env.Path + "\n" + env.Live + "\n"
	if got := read(t, answer); got != want {
		t.Errorf("the command saw\n%q\nand should have seen\n%q", got, want)
	}
}

// A command that is not there at all is a different piece of news from
// one that ran and failed, and it says so.
func TestACommandThatCannotBeStartedIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := with(t, env, "  - command: [\""+
		filepath.Join(env.Path, "no-such-program")+"\"]\n")

	message := mustRefuse(t, prepare.Run(cfg), "prepare-run")
	if !strings.Contains(message, "could not be started") {
		t.Errorf("the refusal should say it never ran:\n%s", message)
	}
}

// The timeout kills the process group, not the process camp started: a
// command that has already forked would otherwise leave its children
// running while camp reported it dealt with.
func TestATimeoutKillsTheWholeGroup(t *testing.T) {
	env := testenv.NewEnv(t)
	marker := filepath.Join(env.Path, "after-the-sleep")
	cfg := with(t, env, "  - command: [\"/bin/sh\", \"-c\", "+
		"\"sleep 30; : > "+marker+"\"]\n    timeout: 1\n")

	message := mustRefuse(t, prepare.Run(cfg), "prepare-timeout")
	if !strings.Contains(message, "process group") {
		t.Errorf("the refusal should say what was killed:\n%s", message)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Error("the sleep outlived its timeout: the kill reached the shell " +
			"camp started and not the group, so the child carried on")
	}
}

// Nothing declared is not a failure, and it costs nothing.
func TestAConfigurationWithNoPrepareCommandsRunsNothing(t *testing.T) {
	env := testenv.NewEnv(t)
	if refused := prepare.Run(env.Config(t, "")); !refused.Empty() {
		t.Fatalf("a configuration with no prepare: was refused:\n%v", refused.Error())
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}
