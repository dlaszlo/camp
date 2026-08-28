// Package runx starts a program the configuration named, and reports how
// it ended.
//
// One place for it, because camp runs configured code at one phase and
// the awkward parts are the same wherever it is started from.
//
// The awkward parts. The program gets its own process group, so that a
// timeout can end the whole tree rather than a parent that has already
// forked -- which takes it out of camp's foreground group, and then an
// interrupt at the terminal reaches camp alone. camp would exit while the
// tree it started carried on writing, which is the opposite of what the
// separate group was for. So the signal is forwarded to that group, and
// camp waits for the process it started before it says anything.
//
// It waits for that process and not for the group, and the difference is
// worth stating rather than glossing: a grandchild that ignores the
// forwarded signal outlives camp. Waiting for the group would mean
// waiting without end on exactly the process that refuses to end, which
// is a hang where there is now a message -- so what camp reports is what
// it did, that the group was signalled.
//
// It words no refusals. Configured code runs for more than one reason,
// the reasons refuse differently, and the caller is the one that knows
// which. What is shared is the mechanism, and only the mechanism is here.
package runx

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// received reports whether a signal is waiting, without blocking for one.
func received(interrupts <-chan os.Signal) (syscall.Signal, bool) {
	select {
	case waiting := <-interrupts:
		if signalled, ok := waiting.(syscall.Signal); ok {
			return signalled, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// Command is one configured program and everything it is given.
type Command struct {
	// Argv is executed directly. There is no shell in between.
	Argv []string
	// Dir is the working directory. Never empty: what a program does with
	// a relative path is the caller's decision to make deliberately.
	Dir string
	// Env is the whole environment the program gets, not an addition to
	// camp's own.
	Env []string
	// Timeout kills the process group when it expires. Zero means none.
	Timeout time.Duration
	// Stdout and Stderr are the program's. stdin is always /dev/null:
	// configured code is not asked questions.
	Stdout, Stderr io.Writer
}

// Outcome is how the program ended, as one of five things. It is a
// string so that a caller switching on it reads as prose and a test that
// reports one says something.
type Outcome string

const (
	// OK: it exited zero.
	OK Outcome = "ok"
	// NotStarted: it never ran. Err says why.
	NotStarted Outcome = "not started"
	// Failed: it ran and ended badly. Err carries the exit status.
	Failed Outcome = "failed"
	// TimedOut: the timeout expired and the process group was killed.
	TimedOut Outcome = "timed out"
	// Interrupted: camp was signalled, the process group was sent the
	// same signal, and camp waited for it.
	Interrupted Outcome = "interrupted"
)

// Result is what happened. Err is set for NotStarted and Failed, Signal
// for Interrupted; nothing else is filled in for the other outcomes.
type Result struct {
	Outcome Outcome
	Err     error
	Signal  syscall.Signal
}

// Run starts the command and does not return while the process it
// started is alive.
func Run(command Command) Result {
	if len(command.Argv) == 0 {
		return Result{Outcome: NotStarted, Err: errors.New("no command to run")}
	}

	// Subscribed before anything is started, and this is load-bearing.
	// Notifying afterwards leaves a window in which the default
	// disposition still stands: a signal arriving there ends camp, while
	// the program camp had just started is in a process group of its own
	// and carries on -- writing, in prepare's case, into repositories,
	// with nothing left to say so. The channel is buffered, so a signal
	// that lands in the window between here and the loop below is
	// delivered rather than dropped.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(interrupts)

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return Result{Outcome: NotStarted, Err: err}
	}
	defer devnull.Close()

	process := exec.Command(command.Argv[0], command.Argv[1:]...)
	process.Dir = command.Dir
	process.Env = command.Env
	process.Stdin = devnull
	process.Stdout = command.Stdout
	process.Stderr = command.Stderr
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Asked to stop before anything was started: then nothing is started,
	// and this is where to ask, with nothing left to do but start. It is
	// reported as never having run rather than as an interruption,
	// because an interruption's message is about a process group that was
	// sent the signal, and there is no group.
	if signalled, pending := received(interrupts); pending {
		return Result{Outcome: NotStarted, Err: fmt.Errorf(
			"camp was sent %s before this command was started", signalled)}
	}

	if err := process.Start(); err != nil {
		return Result{Outcome: NotStarted, Err: err}
	}
	group := -process.Process.Pid

	done := make(chan error, 1)
	go func() { done <- process.Wait() }()

	var expired <-chan time.Time
	if command.Timeout > 0 {
		timer := time.NewTimer(command.Timeout)
		defer timer.Stop()
		expired = timer.C
	}

	for {
		select {
		case err := <-done:
			// A signal that arrived while the process was ending would
			// otherwise be lost here -- this case wins the race, the
			// subscription is dropped on the way out, and camp goes on to
			// compose after somebody asked it to stop. So the subscription
			// is closed first and the channel read once more: between those
			// two acts nothing further can be delivered, so what is in it is
			// everything that arrived. Whichever way the process ended,
			// being asked to stop is the news.
			signal.Stop(interrupts)
			if signalled, pending := received(interrupts); pending {
				// The process camp started is gone; whatever it left behind
				// in its group is not, and the promise is that the group is
				// sent the signal.
				_ = syscall.Kill(group, signalled)
				return Result{Outcome: Interrupted, Signal: signalled}
			}
			if err != nil {
				return Result{Outcome: Failed, Err: err}
			}
			return Result{Outcome: OK}

		case <-expired:
			_ = syscall.Kill(group, syscall.SIGKILL)
			<-done
			return Result{Outcome: TimedOut}

		case received := <-interrupts:
			signalled, ok := received.(syscall.Signal)
			if !ok {
				continue
			}
			_ = syscall.Kill(group, signalled)
			// Waiting first is the point: what must not happen is camp
			// returning while the process it started keeps writing.
			<-done
			return Result{Outcome: Interrupted, Signal: signalled}
		}
	}
}
