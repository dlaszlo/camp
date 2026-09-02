package session

import (
	"fmt"
	"math"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/nsx"
)

// JoinedArg is the hidden argument nsenter execs camp with inside a session
// it has already joined. It is defined in nsx so that the discoverer and
// this side can both name it without a package cycle.
const JoinedArg = nsx.JoinedArg

// JoinedMain unpacks the joined process's argument vector and hands over.
//
// The binary dispatches to this before anything else, like InitMain: by
// the time this runs the process is already inside the session's user,
// mount and pid namespaces (nsenter put it there), so /proc/1 is the
// session's init and the composed tree is what the tree shows. There is no
// handshake and no lock: the pid namespace binds this process's lifetime to
// the init's, so the init already holds the locks for exactly as long as
// anything joined can be alive.
//
// The vector is <config> <live> -- <argv...>. The live path is carried
// across from the joiner, which verified it against the running session's
// mount table and lock descriptors; deriving it here again from the file
// would trust a file that may have been edited since, and send the joined
// process to a directory outside the tree that was verified.
func JoinedMain(args []string) {
	if len(args) < 3 || args[2] != "--" {
		os.Stderr.WriteString("camp: the joined process was invoked wrongly\n")
		os.Exit(1)
	}
	Joined(args[0], args[1], args[3:])
}

// Joined is the init's last step without the init: resolve the session's
// environment against this terminal's own, select the shell or the command,
// take the terminal's foreground, and become it.
//
// The prepare commands and the generation step do not run here, and the
// reason is not economy: a join enters a composition that already exists,
// built once from one reading of the file, and re-running generation would
// rewrite files under a live session. Its environment is §6's declarations
// resolved against this joiner's own inherited environment -- $HOME, $PATH
// and the rest mean "this terminal's" -- because the first shell's
// effective environment is stored nowhere camp could read it. The
// declarations are the one thing read from the file as it now stands (a
// change since the session started is reported by 'camp status'); the live
// path is not, it is the one discovery verified.
func Joined(configPath, live string, argv []string) {
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "camp: "+format+"\n", args...)
		os.Exit(1)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fail("the configuration %s could not be read for the joined shell: %v",
			configPath, err)
	}

	// Against this joiner's own inherited environment, by the same Resolve
	// the init uses -- so a joined shell gets the composition's PATH prepend,
	// its GIT_SSH_COMMAND, and CAMP_LIVE and PWD set to the live path.
	environment, err := Resolve(cfg, live, os.Environ())
	if err != nil {
		fail("%v", err)
	}
	workload, err := environment.Workload(live, argv)
	if err != nil {
		fail("%v", err)
	}

	// Every descriptor above the standard three closes on the exec below.
	// What arrives here beyond them is the three namespace files nsenter was
	// handed, and a shell has no use for a reference to the namespaces it is
	// standing in. Marked close-on-exec rather than closed, so nothing the
	// runtime holds is pulled out from under it before the exec.
	_ = unix.CloseRange(3, math.MaxUint32, unix.CLOSE_RANGE_CLOEXEC)

	// The joined process takes the terminal's foreground group, so Ctrl-C
	// reaches it and not nsenter or the joiner, which are then in a
	// background group of this terminal. Without a terminal there is nothing
	// to take, and the command runs as it stands.
	foreground()

	if err := os.Chdir(workload.Dir); err != nil {
		fail("the composed tree %s could not be entered: %v", workload.Dir, err)
	}
	if err := syscall.Exec(workload.Path, workload.Argv, workload.Env); err != nil {
		fail("running %s: %v", workload.Argv[0], err)
	}
}

// foreground makes this process the foreground group on its terminal.
//
// setpgid(0,0) puts it in a group of its own, and tcsetpgrp hands that
// group the terminal. tcsetpgrp from a background group would raise
// SIGTTOU on the caller, so it is ignored across the call and then reset to
// the default the shell about to be execed will manage for itself -- by
// which point this process is the foreground group and no SIGTTOU can fire.
func foreground() {
	if !terminal(os.Stdin) {
		return
	}
	if err := unix.Setpgid(0, 0); err != nil {
		return
	}
	signal.Ignore(unix.SIGTTOU)
	_ = unix.IoctlSetPointerInt(int(os.Stdin.Fd()), unix.TIOCSPGRP, os.Getpid())
	signal.Reset(unix.SIGTTOU)
}
