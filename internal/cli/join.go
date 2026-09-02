package cli

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/dlaszlo/camp/internal/join"
	"github.com/dlaszlo/camp/internal/refusal"
)

// enterJoined joins a running session of this configuration and runs a
// shell or a command inside it.
//
// It is camp's 'docker exec': it builds nothing, mounts nothing and locks
// nothing. The composition already exists and the init already holds its
// locks; a join adds no composition, and the pid namespace binds the
// joined process's lifetime to the init's, so there is no interval in which
// a joined process exists and the locks do not (§13, §14). The join enters
// through util-linux nsenter, which camp hands the session's namespace
// descriptors: a Go process cannot setns into a user namespace itself,
// being multithreaded before its own code runs.
func enterJoined(ctx *context, file string, argv []string) error {
	cfg, err := resolve(ctx, file)
	if err != nil {
		return err
	}

	target, refused := join.Find(cfg)
	if !refused.Empty() {
		return joinRefused(refused)
	}
	defer target.Close()

	// Only once there is a session to join is a missing tool worth saying:
	// with no session the answer is 'camp shell', tool or no tool.
	nsenter, missing, ok := join.Nsenter()
	if !ok {
		return &Error{Code: joinExit(missing.Rule), Message: missing.Message}
	}

	self, err := os.Executable()
	if err != nil {
		return wrap(fmt.Errorf("finding this binary: %w", err), ExitFailure, "")
	}

	cmd := exec.Command(nsenter, join.JoinArgs(self, cfg.Source, cfg.Live(), argv)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The three namespace descriptors become the child's fds 3, 4, 5, which
	// is the order JoinArgs names them to nsenter.
	cmd.ExtraFiles = target.Files()
	if err := cmd.Start(); err != nil {
		return failure(ExitFailure,
			"the session is running and was not touched; 'camp doctor' reports "+
				"whether nsenter is usable here",
			"nsenter could not be started to join the session: %v", err)
	}
	// This process's copies go the moment the child has its own. An open
	// descriptor to a mount namespace keeps that namespace and the overlay in
	// it alive after every process has left it: a joiner still holding one
	// when the session ended -- stopped, say -- would hold the old overlay
	// open on an upper whose lock the init had already released, and a new
	// start could mount a second overlay on the same upper (C8). What
	// remains is nsenter itself, which stands in the mount namespace for as
	// long as it lives and exits the moment its child is gone.
	target.CloseFiles()
	_ = cmd.Wait()
	status, signalled := joinStatus(cmd)

	// A joined shell's own exit does not end the session; the session goes
	// on. The other direction is the one that can reach here: when the
	// workload ends, the init's fan-out reaches this joined process too, and
	// whatever ignores it is ended by the kernel with the init's exit. Either
	// way, if the init is gone the shell went with the session, and that is
	// said in one line so the sudden exit is not a mystery. A signalled exit
	// is given a moment for the init's own exit to complete (Target.Ended
	// says why); an ordinary exit is answered at once.
	wait := time.Duration(0)
	if signalled {
		wait = time.Second
	}
	if target.Ended(wait) {
		fmt.Fprintln(ctx.err, "the session ended; this shell went with it.")
	}

	if status != 0 {
		os.Exit(status) // the joined process's own status, in nsenter's convention
	}
	return nil
}

// joinStatus reads nsenter's exit the way a shell would report it: the
// process's own code, or 128+signal when it died of one. nsenter re-raises
// a child's fatal signal on itself, so this is the joined process's status
// -- and nsenter's own failures (a namespace it could not join, EPERM
// included) arrive the same way, as its exit status with its own message
// on stderr; camp reads no program's output and does not restate them.
func joinStatus(cmd *exec.Cmd) (status int, signalled bool) {
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return 128 + int(ws.Signal()), true
		}
		return ws.ExitStatus(), false
	}
	return cmd.ProcessState.ExitCode(), false
}

// joinRefused turns a join refusal into the terminal error, carrying the
// exit code the design gives that rule so a script can branch on it.
func joinRefused(refused refusal.List) error {
	first := refused[0]
	return &Error{Code: joinExit(first.Rule), Message: first.Message}
}

// joinExit maps a join refusal to its exit code: no session is "no such
// composition"; being inside one already is "busy"; an init camp could not
// read is a plain failure; the rest are preconditions.
func joinExit(rule string) int {
	switch rule {
	case "join-no-session":
		return ExitNotFound
	case "join-from-inside", "join-from-another-session":
		return ExitBusy
	case "join-init-unreadable":
		return ExitFailure
	default:
		return ExitPrecondition
	}
}
