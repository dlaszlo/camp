// Package session is the namespace mode: the primary way camp runs.
//
// A session is two processes, and the split between them is the whole
// design.
//
// **The launcher** is the command the user typed. It takes the two locks,
// validates the configuration, runs the gate, generates the artefacts and
// validates them -- all as the user, with nothing privileged existing
// yet. Then it clones the init, hands it the lock descriptors and a pipe,
// and waits.
//
// **The init** is camp resident as pid 1 of a new pid namespace. It
// writes the identity maps, gives the namespace its own /proc, performs
// the mount sequence, verifies it, drops the mount capability, and only
// then starts the workload. It stays alive for the whole session: it
// reaps everything that reparents to it, and it holds the locks.
//
// That last part is why the init exists at all. A daemonising program --
// tmux among them -- routinely closes the descriptors it inherited, so a
// design that let the workload carry the locks would be trusting the
// workload's habits. Instead the tmux server reparents to camp-as-init
// inside the namespace, the init lives exactly as long as the composition
// does, and the locks are released by the kernel whatever happens to it.
// No staleness is possible.
//
// It also makes 'camp run -- tmux new-session -d' behave: the tmux client
// exits at once, the launcher exits with its status, and the init stays
// resident holding the composition open for every terminal that attaches
// later.
//
// There is no down here, and no state record. When the last process in
// the namespace exits the kernel discards the namespace and every mount
// in it: teardown cannot fail, there is nothing to hold it open, and no
// half-removed state to reason about. What a session does leave is its
// end-of-session report, which is output rather than authority.
package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/capsx"
	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/locks"
	"github.com/dlaszlo/camp/internal/nsx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
)

// InitArg is the hidden argument that marks the re-executed child. It is
// not a command anyone should type and it is not advertised.
const InitArg = "__init"

// Descriptor numbers the init inherits. They are fixed rather than
// discovered because both halves are this same binary and the contract
// between them is not worth making configurable.
const (
	pipeFD  = 3
	upperFD = 4
	liveFD  = 5
)

// message is one line of the handshake, JSON so that a refusal's own
// newlines survive the trip.
type message struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
	Code int    `json:"code,omitempty"`
}

const (
	kindUp      = "up"
	kindRefused = "refused"
	kindExit    = "exit"
)

// Options is what the launcher was asked to do.
type Options struct {
	Config config.Config
	Plan   plan.Plan
	// Exclude is the validated payload, empty when nothing generates one.
	Exclude []byte
	// Argv is the workload. Empty means an interactive shell.
	Argv []string
	// Locks are held by the launcher and handed to the init.
	Locks *locks.Pair
	// Stdin, Stdout and Stderr are the workload's, passed through.
	Stdin, Stdout, Stderr *os.File
}

// Launch starts the session and returns the workload's exit status.
//
// It waits not for the init but for the workload, whose status the init
// sends back over the pipe. That is what makes a foreground command
// behave like a foreground command and a daemonising one return at once.
func Launch(options Options) (int, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return 1, fmt.Errorf("opening the handshake pipe: %w", err)
	}
	defer read.Close()

	identity := nsx.For(options.Config.Identity)
	attrs, err := identity.Attrs()
	if err != nil {
		write.Close()
		return 1, err
	}

	self, err := os.Executable()
	if err != nil {
		write.Close()
		return 1, fmt.Errorf("finding this binary: %w", err)
	}

	argv := append([]string{InitArg, options.Config.Source,
		strconv.Itoa(identity.InsideUID), strconv.Itoa(identity.InsideGID), "--"},
		options.Argv...)
	cmd := exec.Command(self, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = options.Stdin, options.Stdout, options.Stderr
	cmd.SysProcAttr = attrs
	cmd.ExtraFiles = append([]*os.File{write}, options.Locks.Files()...)
	cmd.Env = append(os.Environ(), "CAMP_SESSION="+options.Plan.Hash)

	if err := cmd.Start(); err != nil {
		write.Close()
		return 1, namespaceError(err)
	}
	// The write end belongs to the init now. Holding a copy here would
	// mean this process never sees the pipe close when the init dies.
	write.Close()

	if identity.Route == config.UIDMap {
		if err := identity.WriteMaps(cmd.Process.Pid); err != nil {
			_ = cmd.Process.Kill()
			return 1, err
		}
	}

	status := 1
	scanner := bufio.NewScanner(read)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sawUp := false
	for scanner.Scan() {
		var note message
		if err := json.Unmarshal(scanner.Bytes(), &note); err != nil {
			continue
		}
		switch note.Kind {
		case kindUp:
			sawUp = true
			// The locks live on the descriptions the init inherited. This
			// process can let go of its own copies now.
			options.Locks.Release()
		case kindRefused:
			return 1, refusal.New("session-refused", "%s", note.Text)
		case kindExit:
			status = note.Code
		}
	}

	if !sawUp {
		return 1, fmt.Errorf("the session ended before the composition was up, " +
			"and said nothing about why")
	}
	return status, nil
}

func namespaceError(err error) error {
	if errors.Is(err, os.ErrPermission) || errors.Is(err, unix.EPERM) {
		return refusal.New("namespace-denied",
			"this kernel refused to create a user namespace.\n"+
				"Run 'camp doctor': it says which switch is set and how to change "+
				"it. On Ubuntu 23.10 and later the permission is granted by an "+
				"AppArmor profile to one installed binary path, and a copy of the "+
				"binary anywhere else is not covered by it.")
	}
	return fmt.Errorf("starting the session: %w", err)
}

// Inside is the init: camp as pid 1 of the namespace.
func Inside(configPath string, insideUID, insideGID int, argv []string) {
	pipe := os.NewFile(pipeFD, "handshake")
	report := func(note message) {
		encoded, err := json.Marshal(note)
		if err != nil {
			return
		}
		pipe.Write(append(encoded, '\n'))
	}
	refuse := func(format string, args ...any) {
		report(message{Kind: kindRefused, Text: fmt.Sprintf(format, args...)})
		os.Exit(1)
	}

	// The locks arrived as descriptors. Adopting them is only about having
	// a handle: the lock is on the open file description, which this
	// process already holds by having inherited it.
	upper := locks.Adopt(locks.Upper, "upper", upperFD)
	live := locks.Adopt(locks.Live, "live", liveFD)
	defer upper.Release()
	defer live.Release()

	built, exclude, problems := rebuild(configPath)
	if !problems.Empty() {
		refuse("%s", problems.Error())
	}

	if err := nsx.Detach(); err != nil {
		refuse("%v", err)
	}
	if err := nsx.MountProc(); err != nil {
		refuse("%v", err)
	}

	setup := compose.Setup{
		Plan:    built,
		Prefix:  built.Live,
		Exclude: exclude,
		UID:     insideUID,
		GID:     insideGID,
	}
	result := compose.Build(setup)
	if !result.OK() {
		refuse("%s", describeFailure(result))
	}

	// Everything is mounted and verified. The capability goes back before
	// anything the user asked for runs.
	if err := capsx.Drop(); err != nil {
		refuse("giving the mount capability back: %v", err)
	}

	workload, err := startWorkload(built.Live, argv)
	if err != nil {
		refuse("%v", err)
	}
	report(message{Kind: kindUp})

	status := supervise(workload)
	report(message{Kind: kindExit, Code: status})
	pipe.Close()
	os.Exit(0)
}

// rebuild derives the plan again inside the namespace.
//
// The launcher already validated everything; this re-derives rather than
// serialising the plan across, because the two halves are the same binary
// reading the same file and a second derivation that disagreed would be a
// bug worth finding here rather than a format worth maintaining.
func rebuild(configPath string) (plan.Plan, []byte, refusal.List) {
	var refused refusal.List
	cfg, err := config.Load(configPath)
	if err != nil {
		var list refusal.List
		if errors.As(err, &list) {
			return plan.Plan{}, nil, list
		}
		refused.Add("config", "%v", err)
		return plan.Plan{}, nil, refused
	}

	built, problems := plan.Prepare(cfg, plan.Namespace)
	if !problems.Empty() {
		return plan.Plan{}, nil, problems
	}

	// Checked again here, by the process that is about to mount it, and
	// against the repositories themselves. The launcher already validated
	// the same output; doing it again is what closes the gap between the
	// moment of validation and the moment of mounting, and it is what lets
	// verification compare the mounted file against a payload computed
	// from the repositories rather than against the file it just mounted.
	generated, problems := gen.Adopt(built)
	if !problems.Empty() {
		return plan.Plan{}, nil, problems
	}
	return gen.Expand(built, generated), generated.Exclude, refused
}

func describeFailure(result compose.Result) string {
	text := result.Refused.Error()
	if len(result.Stranded) > 0 {
		text += fmt.Sprintf("\n\nand the rollback could not finish: %v are still "+
			"mounted inside this session's namespace. They go when it ends.",
			result.Stranded)
	}
	return text
}

// startWorkload runs what the user asked for, inside the composed tree.
func startWorkload(live string, argv []string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		argv = []string{shell()}
	}
	binary, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, fmt.Errorf("cannot run %q: %w", argv[0], err)
	}

	cmd := exec.Command(binary, argv[1:]...)
	cmd.Dir = live
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "CAMP_LIVE="+live, "PWD="+live)

	// The workload gets its own process group, and where there is a
	// terminal it becomes the foreground group on it. That is what makes
	// Ctrl-C reach the workload instead of the supervisor holding the
	// locks.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if terminal(os.Stdin) {
		cmd.SysProcAttr.Foreground = true
		cmd.SysProcAttr.Ctty = int(os.Stdin.Fd())
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %q: %w", argv[0], err)
	}
	return cmd, nil
}

func terminal(file *os.File) bool {
	_, err := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS)
	return err == nil
}

func shell() string {
	if configured := os.Getenv("SHELL"); configured != "" {
		return configured
	}
	return "/bin/sh"
}

// supervise is the init's remaining life.
//
// It reaps everything that reparents to it, which as pid 1 of the
// namespace is everything the session ever starts. It forwards SIGTERM
// and SIGHUP to the workload's process group, and it deliberately ignores
// SIGINT, SIGQUIT, SIGTTIN and SIGTTOU: a Ctrl-C is for the workload, and
// it must never kill the supervisor that is holding the locks in the
// middle of a session.
//
// It returns when the last other process in the namespace is gone, with
// the workload's exit status.
func supervise(workload *exec.Cmd) int {
	ignored := make(chan os.Signal, 1)
	signal.Notify(ignored, unix.SIGINT, unix.SIGQUIT, unix.SIGTTIN, unix.SIGTTOU)
	go func() {
		for range ignored {
		}
	}()

	forwarded := make(chan os.Signal, 1)
	signal.Notify(forwarded, unix.SIGTERM, unix.SIGHUP)
	go func() {
		for received := range forwarded {
			if signal, ok := received.(syscall.Signal); ok && workload.Process != nil {
				_ = unix.Kill(-workload.Process.Pid, signal)
			}
		}
	}()

	status := 0
	for {
		var wait unix.WaitStatus
		pid, err := unix.Wait4(-1, &wait, 0, nil)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ECHILD):
			// Nothing is left in the namespace. The session is over.
			return status
		case err != nil:
			return status
		}
		if workload.Process != nil && pid == workload.Process.Pid {
			status = exitStatus(wait)
		}
	}
}

func exitStatus(wait unix.WaitStatus) int {
	if wait.Signaled() {
		return 128 + int(wait.Signal())
	}
	return wait.ExitStatus()
}
