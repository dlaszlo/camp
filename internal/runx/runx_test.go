package runx_test

import (
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dlaszlo/camp/internal/runx"
)

// runx starts programs camp did not write. What is measured here is how
// it tells one ending from another, and what it leaves running.

func command(t *testing.T, argv ...string) runx.Command {
	t.Helper()
	return runx.Command{Argv: argv, Dir: t.TempDir(), Env: os.Environ(),
		Stdout: os.Stdout, Stderr: os.Stderr}
}

func TestHowARunEndedIsReportedAsWhatHappened(t *testing.T) {
	for _, one := range []struct {
		name string
		argv []string
		want runx.Outcome
	}{
		{"a program that exits zero", []string{"/bin/true"}, runx.OK},
		{"a program that exits non-zero", []string{"/bin/false"}, runx.Failed},
		{"a program that is not there", []string{"/nonexistent/program"}, runx.NotStarted},
	} {
		t.Run(one.name, func(t *testing.T) {
			if got := runx.Run(command(t, one.argv...)); got.Outcome != one.want {
				t.Errorf("ended as %q, wanted %q (%v)", got.Outcome, one.want, got.Err)
			}
		})
	}
}

// An empty vector is refused here rather than reaching the index that
// would panic. Both callers validate their configuration first, so this
// cannot arrive today -- which is exactly the assumption that stops being
// true without anybody noticing.
func TestAnEmptyCommandIsRefusedRatherThanIndexed(t *testing.T) {
	result := runx.Run(runx.Command{Dir: t.TempDir()})
	if result.Outcome != runx.NotStarted {
		t.Errorf("an empty argument vector ended as %q", result.Outcome)
	}
}

// The timeout kills the process group, not the one process camp started:
// a program that has already forked would otherwise leave its children
// running while camp reported it dealt with.
func TestATimeoutEndsTheGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "after-the-sleep")
	run := command(t, "/bin/sh", "-c", "sleep 30; : > "+marker)
	run.Timeout = 300 * time.Millisecond

	if got := runx.Run(run); got.Outcome != runx.TimedOut {
		t.Fatalf("ended as %q, wanted %q", got.Outcome, runx.TimedOut)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Error("the sleep outlived the timeout: the kill reached the shell " +
			"and not its group")
	}
}

// An interrupt aimed at camp reaches the program too. It has to: camp put
// it in a process group of its own so a timeout could end the whole tree,
// and that is exactly what takes it out of the group a terminal's Ctrl-C
// reaches. Without the forward, camp would exit while the tree it started
// carried on writing.
func TestAnInterruptReachesTheProgramAndEndsTheRun(t *testing.T) {
	// The test binary holds its own subscription for the whole test, so
	// that a signal arriving before or after Run's own can never fall to
	// the default disposition and end the test binary itself.
	mine := make(chan os.Signal, 1)
	signal.Notify(mine, syscall.SIGINT)
	defer signal.Stop(mine)

	marker := filepath.Join(t.TempDir(), "after-the-sleep")
	go func() {
		for _, wait := range []time.Duration{200, 700} {
			time.Sleep(wait * time.Millisecond)
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGINT)
		}
	}()

	result := runx.Run(command(t, "/bin/sh", "-c", "sleep 30; : > "+marker))
	if result.Outcome != runx.Interrupted {
		t.Fatalf("ended as %q, wanted %q", result.Outcome, runx.Interrupted)
	}
	if result.Signal != syscall.SIGINT {
		t.Errorf("reported the signal as %v, and it was an interrupt", result.Signal)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Error("the program ran to completion: the interrupt reached camp " +
			"and not the group camp started")
	}
	// Whatever is left in either channel belongs to this test's own
	// subscription, and a later test should not meet it.
	drain(mine)
}

func drain(signals chan os.Signal) {
	for {
		select {
		case <-signals:
		default:
			return
		}
	}
}
