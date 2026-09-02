// Package session builds the composition inside a namespace of its own
// and supervises what runs there. It is the one way camp composes.
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
// That last part is why the init exists at all. A daemonising program
// routinely closes the descriptors it inherited, so a design that let the
// workload carry the locks would be trusting the workload's habits.
// Instead the locks live on the init, the init lives exactly as long as
// the composition does, and the locks are released by the kernel whatever
// happens to it. No staleness is possible.
//
// **A session ends when its workload ends** -- the shell 'camp shell'
// opened, or the command 'camp run' was given. Not when its last process
// is gone: what is still inside at that moment is asked to leave (SIGTERM
// to every process in the namespace, SIGCONT behind it), given Grace to
// do so, and then the init exits and the kernel ends the remainder. The
// init acts on its own observation of the exit, never on a message from
// the launcher: a launcher can be killed, nohup'ed or lose its terminal,
// and the session it started is still the session.
//
// There is no teardown command, and no state record. When the init exits
// the kernel ends every other process in the pid namespace and discards
// the namespace with every mount in it: teardown cannot fail, there is
// nothing to hold it open, and no half-removed state to reason about.
// What a session does leave is its end-of-session report, which is output
// rather than authority.
package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/capsx"
	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/drift"
	"github.com/dlaszlo/camp/internal/envx"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/locks"
	"github.com/dlaszlo/camp/internal/logs"
	"github.com/dlaszlo/camp/internal/nsx"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/report"
	"github.com/dlaszlo/camp/internal/reports"
)

// InitArg is the hidden argument that marks the re-executed child. It is
// not a command anyone should type and it is not advertised. Defined
// beside the namespace (nsx) so that a lock refusal can name the init
// among a directory's holders without importing this package.
const InitArg = nsx.InitArg

// Grace is how long the init waits, after its workload has exited, for
// the rest of the namespace to act on the SIGTERM it was sent.
//
// It exists for a program that answers SIGTERM by saving and exiting: a
// shell writing its history, an editor its swap file, a browser its
// profile. The first two take a fraction of a second, the third a few
// seconds. Ten is a generous multiple of an honest shutdown and a short
// enough time for a person to sit through once; it is also the stop grace
// programs meant to be stopped are written against (Docker's). In the
// ordinary case -- a shell that exits with nothing behind it -- the wait
// is zero, because the first look finds the namespace empty.
//
// A constant, not a configuration key, because no environment needs
// another value yet and a knob before its caller is speculation. The
// place it would go is settled so the question does not reopen: a
// session.grace key in the configuration, in seconds, and never a
// command-line flag -- how long an environment's programs take to stop is
// a property of the environment. Every message that depends on it prints
// it, so nobody has to find this constant to know what happened.
const Grace = 10 * time.Second

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
//
// The plan and the generated payload are deliberately not here. The init
// derives both again from the configuration, inside the namespace, and
// that is the point: it is the process that mounts, so what it checked
// and what it mounts cannot be two different things. Carrying the
// launcher's copy across as well would be a second source of truth for
// the same question, and the one that had not been re-checked. What the
// init does check is that the plan it derived is about the directories
// the inherited locks are on.
type Options struct {
	Config config.Config
	// Argv is the workload. Empty means an interactive shell.
	Argv []string
	// Locks are held by the launcher and handed to the init.
	Locks *locks.Pair
	// Stdin, Stdout and Stderr are the workload's, passed through.
	Stdin, Stdout, Stderr *os.File
}

// Launch starts the session, waits for it to end, and returns the
// workload's exit status.
//
// The status arrives over the pipe the moment the workload exits; the
// return waits for the init to exit, however that came. The init's exit
// is bounded by Grace, and a kill -9 of the init ends it as surely as an
// exit does, so this process needs no timeout of its own and is never
// left waiting on something unbounded.
//
// It waits for the init itself -- its own child, through wait4 -- and not
// only for the pipe to close, and the difference is measured: a launcher
// that returned on the pipe's close found the locks still held, because
// the init was between closing the pipe and being gone. When wait4 on a
// pid namespace's init returns, the kernel has finished its exit path:
// its descriptors are closed, so the locks are released; every other
// process in the namespace has been ended and reaped; and the mounts went
// with the namespace. So when this returns the session is over in every
// sense the next command in this environment could ask about.
func Launch(options Options) (int, error) {
	// The terminal says a session is running for as long as one is, and
	// says nothing afterwards. Restored when this returns, which is when
	// the session is over.
	defer nameTerminal(options.Stderr, options.Config.Env)()

	read, write, err := os.Pipe()
	if err != nil {
		return 1, fmt.Errorf("opening the handshake pipe: %w", err)
	}
	defer read.Close()

	identity := nsx.For(options.Config.Session.Identity)
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
			_ = cmd.Wait()
			return 1, err
		}
	}

	status := 1
	scanner := bufio.NewScanner(read)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sawUp, sawExit := false, false
	var refused error
	for scanner.Scan() && refused == nil {
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
			refused = refusal.New("session-refused", "%s", note.Text)
		case kindExit:
			// The workload is finished. The session is not, quite: the init
			// is running its end-of-session pass and asking whatever is left
			// inside to leave, each bounded by Grace. Read on until the pipe
			// closes, then wait for the init itself.
			status, sawExit = note.Code, true
		}
	}
	// The init's own status is not the answer -- a refusal already came
	// over the pipe, and the workload's status is what is returned -- so
	// only the fact of its exit is waited for. On a refusal too: the init
	// holds the locks until it is gone, and a retry typed the moment the
	// refusal is printed must meet a free composition, not the old init.
	_ = cmd.Wait()
	if refused != nil {
		return 1, refused
	}

	if !sawUp {
		return 1, fmt.Errorf("the session ended before the composition was up, " +
			"and said nothing about why")
	}
	if !sawExit {
		return 1, fmt.Errorf("the session ended before its workload did, and " +
			"said nothing about why: the init was gone before it could report " +
			"the workload's exit status. If it was killed, that was the end of " +
			"the session; the kernel has ended everything that was inside it")
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

// InitMain unpacks the init's argument vector and hands over.
//
// The binary dispatches to this before anything else -- no flag parsing,
// no configuration discovery, no logging in between. The init has one
// job, and it is holding the session's locks while it does it.
func InitMain(args []string) {
	if len(args) < 4 || args[3] != "--" {
		os.Stderr.WriteString("camp: the session init was invoked wrongly\n")
		os.Exit(1)
	}
	uid, uidErr := strconv.Atoi(args[1])
	gid, gidErr := strconv.Atoi(args[2])
	if uidErr != nil || gidErr != nil {
		os.Stderr.WriteString("camp: the session init was given a bad identity\n")
		os.Exit(1)
	}
	Inside(args[0], uid, gid, args[4:])
}

// keepLog attaches this half of the session's log, and says so once if
// it cannot.
//
// The failure is reported rather than stepped over. Most of what a
// session says is said from in here -- the mounts, the verification, the
// identity, the farewell -- so a log that the launcher opened and this
// process could not is a file that holds the four lines before the
// interesting ones and nothing to say why. The session goes ahead: it
// has a workload to start, and a record not being kept is not a reason
// to refuse one.
func keepLog(sink *report.Sink, root pathx.Root) {
	file, err := logs.Open(root, sink)
	if err != nil {
		sink.Trouble("camp's log is not being written from inside the "+
			"session: %v.\nThe session goes ahead. Everything it says from "+
			"here -- the mounts, the verification and the farewell -- is on "+
			"this terminal only, and %s holds the launcher's lines and no "+
			"more.", err, logs.Path(root))
		return
	}
	sink.Keep(file)
}

// Inside is the init: camp as pid 1 of the namespace.
//
// It runs on one thread from here to the workload, and that is not a
// preference. Capabilities in Linux are per *thread*: capset, the ambient
// clear and every bounding drop act on the thread that made the call, and
// nothing else. Go moves a goroutine between threads whenever it likes, so
// a program that drops its capabilities and then starts a child has no
// guarantee that the two happened on the same thread -- and if they did
// not, the child forks from a thread that still holds CAP_SYS_ADMIN and
// inherits the mount capability the drop exists to take away.
//
// Measured, on a machine that exists for one run: the same test passed and
// failed on consecutive runs of unchanged code, reporting the capabilities
// still there after the drop. That is the goroutine having moved.
//
// Locking the thread also puts the drop where it can be seen. /proc/self
// reports the thread group leader, which is the thread the main goroutine
// starts on -- so a drop that happens here is a drop anybody looking at
// this process can read, rather than one that took effect on a worker
// thread nothing names.
func Inside(configPath string, insideUID, insideGID int, argv []string) {
	runtime.LockOSThread()

	pipe := os.NewFile(pipeFD, "handshake")
	send := func(note message) {
		encoded, err := json.Marshal(note)
		if err != nil {
			return
		}
		pipe.Write(append(encoded, '\n'))
	}
	refuse := func(format string, args ...any) {
		send(message{Kind: kindRefused, Text: fmt.Sprintf(format, args...)})
		os.Exit(1)
	}

	// This process is pid 1 of its own pid namespace, or it is not the
	// init and nothing below it is true. Asked rather than assumed because
	// the end of a session rests on it: supervise ends a session with
	// kill(-1), which is every process in the namespace here and every
	// process the user owns anywhere else. The clone carries CLONE_NEWPID
	// (internal/nsx), so this cannot fire; what it buys is that the
	// fan-out is safe to read on its own.
	if pid := os.Getpid(); pid != 1 {
		refuse("the session init is pid %d, and the init is pid 1 of its own "+
			"pid namespace.\nNothing has been mounted. Either this process was "+
			"cloned without CLONE_NEWPID, or something other than camp invoked "+
			"'camp %s'.", pid, InitArg)
	}

	// The locks arrived as descriptors. Adopting them is only about having
	// a handle: the lock is on the open file description, which this
	// process already holds by having inherited it.
	upper := locks.Adopt(locks.Upper, "upper", upperFD)
	live := locks.Adopt(locks.Live, "live", liveFD)
	defer upper.Release()
	defer live.Release()

	// The three inherited descriptors are this process's and stay here.
	// They arrived without close-on-exec, and a workload that inherited
	// them reached the code repository writable at /proc/self/fd/4/... --
	// the lock descriptor was opened through the raw path before the
	// freeze existed, and a path resolved through a descriptor uses the
	// mount the descriptor was opened on -- so the freeze of step 9a was
	// bypassable from inside by anything that looked (measured). The lock
	// lives on the open file description this process keeps; the child
	// needs no copy of it, or of the handshake pipe.
	for _, fd := range []int{pipeFD, upperFD, liveFD} {
		unix.CloseOnExec(fd)
	}

	built, exclude, problems := rebuild(configPath)
	if !problems.Empty() {
		refuse("%s", problems.Error())
	}

	// The plan this process derived has to be about the two directories
	// the inherited locks are on, and that is a question only this process
	// can answer: the launcher locked two inodes and then handed the
	// descriptors over, and the configuration was read again in between.
	// A file edited in that window would have this process mount one upper
	// while camp holds the lock for another -- two compositions on one
	// upper, which is the thing the locks exist to make impossible.
	if err := locksMatch(built, upper, live); err != nil {
		refuse("%v", err)
	}

	// The steps say what they did, in order, on this process's stderr --
	// which is the user's terminal. They come from here rather than from
	// the launcher because this is one sequential process, so the order
	// they appear in is the order things happened in.
	//
	// And into the same log the launcher writes: this half of a session
	// says most of what a session says, and a log that stopped at the
	// handover would hold the four lines before the interesting ones. Two
	// processes append to one file, which is what the lock in the log is
	// for.
	stderr := report.To(os.Stderr)
	defer stderr.Close()
	keepLog(stderr, built.Config.Root)
	say := report.Narrate(stderr)
	say.Identity(built.Config.Session)

	// Resolved here, before anything is mounted, so that a reference the
	// configuration cannot satisfy stops the session while there is still
	// nothing to take apart. What it produces is inert text: nothing
	// declared is installed on this process, whose capability is exactly
	// what must never meet a configured PATH.
	environment, err := Resolve(built.Config, built.Live, os.Environ())
	if err != nil {
		refuse("%v", err)
	}

	if err := nsx.Detach(); err != nil {
		refuse("%v", err)
	}
	if err := nsx.MountProc(); err != nil {
		refuse("%v", err)
	}

	setup := compose.Setup{
		Plan:    built,
		Exclude: exclude,
		UID:     insideUID,
		GID:     insideGID,
	}
	result := compose.Build(setup)
	if !result.OK() {
		refuse("%s", describeFailure(result))
	}
	say.Mounted(len(result.Mounted), built.Live)
	say.Verified(len(result.Mounted), built.Live)

	// Everything is mounted and verified. The capability goes back before
	// anything the user asked for runs -- on this thread, which this
	// function has been locked to since it started, and which is the thread
	// the workload will be forked from. Inside's own comment says why that
	// is load-bearing rather than tidy.
	if err := capsx.Drop(); err != nil {
		refuse("giving the mount capability back: %v", err)
	}
	if left, err := capsx.Read(0); err == nil && !left.Empty() {
		// Read back rather than trusted. Every call the drop makes can
		// answer success for the thread it ran on while the process still
		// holds what it meant to give away, and a session that started a
		// workload on that footing would be the one thing this design
		// promises it never does.
		refuse("the mount capability was given back and this process still "+
			"holds %s.\nNo workload is started on that footing.", left.String())
	}

	// And only now is anything the configuration declared attached to a
	// process, or used to choose one. A failed drop starts no workload at
	// all, and the command lookup needs the composed tree standing anyway:
	// the launcher directory a declared PATH prepends lives inside it.
	say.Environment(environment.Applied)
	workload, err := environment.Workload(built.Live, argv)
	if err != nil {
		refuse("%v", err)
	}
	// The workload is started by the supervisor rather than before it, and
	// the ordering is the point. Subscribing to the signals afterwards
	// left a window with a workload already running, the session already
	// announced as up, and SIGTERM still on the runtime's default path --
	// so the one moment the session was most alive was the one moment its
	// termination contract did not hold.
	//
	// The workload's status goes back the moment the workload exits; the
	// session then ends, and the launcher returns when this process has.
	asked := &request{}
	err = supervise(
		func() (*exec.Cmd, error) {
			started, err := startWorkload(workload)
			if err != nil {
				return nil, err
			}
			send(message{Kind: kindUp})
			return started, nil
		},
		func(status int) { send(message{Kind: kindExit, Code: status}) },
		asked)
	if err != nil {
		refuse("%v", err)
	}

	// The end of a session, in this order: look and report while the tree
	// is still whole and everything inside is still alive, then ask what
	// is left to leave. The report comes first because it reads the tree
	// -- a scan run after the namespace was emptied would be reading a
	// tree whose processes were already gone -- and because deciding that
	// the namespace is empty and then running a scan would leave a process
	// that entered during the scan with no request and no grace, only the
	// kernel's SIGKILL. The launcher that would print the report may be
	// gone by now, which is why it is also written to a file. Both halves
	// are bounded by Grace, so a git that hangs in the scan cannot hold
	// the session open: it is sent SIGTERM, left behind, and then met by
	// the ending as one more process inside.
	farewell(built, stderr, time.Now().Add(Grace))
	what := "the command"
	if len(argv) == 0 {
		what = "the shell"
	}
	ending{what: what, out: stderr, asked: asked}.run()

	os.Exit(0)
}

// locksMatch compares the inherited locks with what the plan says.
//
// By identity and never by path: two names routinely mean one directory,
// and the lock is on the inode. The lock descriptors were opened by the
// launcher, so what they answer is what was locked, whatever the
// configuration says now.
func locksMatch(built plan.Plan, upper, live *locks.Held) error {
	for _, pair := range []struct {
		held *locks.Held
		what string
		path string
	}{
		{upper, "code repository", built.Config.UpperPath()},
		{live, "composed tree's directory", built.Live},
	} {
		locked, err := pair.held.Identity()
		if err != nil {
			return err
		}
		now, err := pathx.StatBeneath(pair.path, nil)
		if err != nil {
			return fmt.Errorf("looking at the %s %s: %w", pair.what, pair.path, err)
		}
		if now.Ident != locked {
			return fmt.Errorf(
				"the %s this session would compose is %s, and the lock this "+
					"session holds is on a different directory (%s against %s).\n"+
					"The configuration was read once to take the locks and once "+
					"again by the process that mounts, and it changed in between. "+
					"Nothing has been mounted. Look at %s, and start the session "+
					"again.",
				pair.what, pair.path, now.Ident, locked, built.Config.Source)
		}
	}
	return nil
}

// farewell runs the read-only end-of-session pass, bounded by the
// deadline, writes what it found where the next camp command will find
// it, and prints it when there is still a terminal attached.
func farewell(built plan.Plan, stderr io.Writer, deadline time.Time) {
	found := drift.Refresh(built, deadline)
	if found.Empty() {
		return
	}
	body := "the session at " + built.Live + " ended.\n\n" + found.String()
	if _, err := reports.Write(built.Config.Root, built.Hash, body); err != nil {
		fmt.Fprintf(stderr, "camp: the end-of-session report could not be "+
			"written: %v\n", err)
	}
	fmt.Fprintf(stderr, "\n%s", body)
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

	built, problems := plan.Prepare(cfg)
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

// Environment is what a session's workload will run with: the effective
// list, and the names the configuration declared.
//
// It is built while the init still holds the mount capability, which is
// safe precisely because building it does nothing but concatenate bytes:
// no file is read, no process is changed, no lookup is performed. It is
// inert until Workload turns it into a command, and Workload runs only
// after the capability has gone back.
type Environment struct {
	// Env is the effective environment: one duplicate-free list.
	Env []string
	// Applied names the declared variables, in the order a report shows
	// them. Never their values.
	Applied []string
}

// Workload is the command a session will start: the executable that was
// selected, its arguments, and the environment it receives.
type Workload struct {
	Path string
	Argv []string
	Env  []string
	// Dir is the composed tree, which is where the workload starts.
	Dir string
}

// Resolve builds the effective environment from an explicit base.
//
// Explicit, never os.Environ() reached for inside: the base is the
// snapshot the launcher was started with, both halves of the session
// resolve against the same one, and nothing here mutates the process it
// runs in. There is no os.Setenv anywhere in this path -- a declared PATH
// installed on camp's own init would be a configured value steering a
// process that holds CAP_SYS_ADMIN.
func Resolve(cfg config.Config, live string, base []string) (Environment, error) {
	declared := make([]envx.Setting, 0, len(cfg.Session.Environment))
	applied := make([]string, 0, len(cfg.Session.Environment))
	resolution := envx.NewBase(base, live)
	for _, declaration := range cfg.Session.Environment {
		value, err := declaration.Expr.Resolve(resolution)
		if err != nil {
			return Environment{}, err
		}
		declared = append(declared, envx.Setting{Name: declaration.Name, Value: value})
		applied = append(applied, declaration.Name)
	}

	// The two camp-owned names, and the whole of the camp-owned contract.
	// There is deliberately no session identifier among them: an exported
	// marker invites host-side wrappers that switch on it, which is wiring
	// the host through another door.
	effective := envx.Effective(base, declared, []envx.Setting{
		{Name: envx.Live, Value: live},
		{Name: envx.Cwd, Value: live},
	})
	return Environment{Env: effective, Applied: applied}, nil
}

// Workload selects the command, against the environment the workload will
// actually have.
//
// This is the part that has to be right, and the part that has to happen
// last. A bare command name is resolved against the *effective* PATH, so
// that the workspace-owned launcher directory a composition prepends is
// really reached -- and it can only be reached once the composition is
// mounted, because that directory lives inside the composed tree.
// Resolving against camp's own path while the plan prints the declared one
// would run the host's command under a plan that says otherwise.
func (e Environment) Workload(live string, argv []string) (Workload, error) {
	if len(argv) == 0 {
		argv = []string{shell(envx.Value(e.Env, "SHELL"))}
	}
	path, err := envx.Command(argv[0], envx.Value(e.Env, "PATH"), live)
	if err != nil {
		return Workload{}, err
	}
	return Workload{Path: path, Argv: argv, Env: e.Env, Dir: live}, nil
}

// startWorkload runs what the user asked for, inside the composed tree.
func startWorkload(workload Workload) (*exec.Cmd, error) {
	cmd := exec.Command(workload.Path, workload.Argv[1:]...)
	cmd.Dir = workload.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = workload.Env

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
		return nil, fmt.Errorf("starting %q: %w", workload.Argv[0], err)
	}
	return cmd, nil
}

func terminal(file *os.File) bool {
	_, err := unix.IoctlGetTermios(int(file.Fd()), unix.TCGETS)
	return err == nil
}

// shell picks the interactive shell from the effective SHELL, not from
// camp's own: a session that declares one has said which shell it means,
// and starting a different one would be the plan describing something
// other than what happened.
func shell(configured string) string {
	if configured != "" {
		return configured
	}
	return "/bin/sh"
}

// supervise is the init's remaining life.
//
// It reaps everything that reparents to it, which as pid 1 of the
// namespace is everything the session ever starts. It deliberately
// ignores SIGINT, SIGQUIT, SIGTTIN and SIGTTOU: a Ctrl-C is for the
// workload, and it must never kill the supervisor that is holding the
// locks in the middle of a session.
//
// A SIGTERM or SIGHUP delivered to this process means "end this
// session", and it reaches every process in the namespace: as pid 1,
// kill(-1) signals all of them and nothing outside, and the kernel leaves
// pid 1 itself out of that fan-out. It used to go to the workload's
// process group alone, which was silent in the one case that actually
// happens -- a workload that has already exited leaves no group to
// signal, while a server that reparented here holds the composition open,
// so the request reached nothing at all.
//
// SIGCONT follows it, and it is not a courtesy. A stopped process keeps a
// SIGTERM pending and stays stopped: it never gets to decide anything,
// and the session it holds open would never end. Continuing it is what
// makes the request a request rather than something that happens to reach
// only the processes that were running.
//
// The session ends when the workload exits, whichever way that came: a
// SIGTERM to this process fans out, the workload dies of it, and its exit
// is the same exit as a shell's 'exit'. What happens then is Inside's:
// the end-of-session pass, then the ending (below), in which what is
// still inside is named, asked -- once per session end, the request
// shared with the handler here -- and given Grace. Nothing is escalated
// to SIGKILL by camp; a process that ignores the request is ended by the
// kernel when this process exits, and the ending says so by pid and
// command.
//
// This is the contract of a session that is running, and it holds from
// the moment there is anything to run: the subscription is taken before
// the workload is started, which is why starting it belongs to this
// function. Earlier than that -- during the mount sequence, with no
// workload in existence -- a signal ends the init under the runtime's own
// disposition and the kernel discards the namespace with every mount in
// it. That is the right answer to "stop" at that point, there being
// nothing to ask and nothing that survives, and it is a different answer,
// which is why it is written down rather than folded into the sentence
// above.
//
// It calls back the moment the workload itself exits and returns; what
// follows -- the end-of-session pass and the ending -- is Inside's, in
// that order.
func supervise(start func() (*exec.Cmd, error), workloadExited func(int), asked *request) error {
	ignored := make(chan os.Signal, 1)
	signal.Notify(ignored, unix.SIGINT, unix.SIGQUIT, unix.SIGTTIN, unix.SIGTTOU)
	go func() {
		for range ignored {
		}
	}()

	// Subscribed before the workload exists, and buffered, so that a
	// signal arriving in the moment between the two is held rather than
	// falling to the default disposition. The forwarding starts once there
	// is something to forward to: a fan-out into an empty namespace would
	// reach nothing and be spent.
	forwarded := make(chan os.Signal, 1)
	signal.Notify(forwarded, unix.SIGTERM, unix.SIGHUP)

	workload, err := start()
	if err != nil {
		return err
	}

	go func() {
		for received := range forwarded {
			if signal, ok := received.(syscall.Signal); ok {
				asked.forward(signal)
			}
		}
	}()

	workloadExited(reapUntil(workload.Process.Pid))
	return nil
}

// request is the fan-out that asks the namespace to end, and the record
// that it was made -- shared between the signal handler and the ending so
// that a session's end asks once.
//
// Without the record a SIGTERM to the init asked twice: the handler fanned
// out, the workload died of it, and the ending fanned out again. A process
// that saves on the first SIGTERM and aborts on the second lost the grace
// it was promised.
type request struct {
	mu   sync.Mutex
	sent bool
}

// forward fans a signal camp was sent out to the namespace: once per
// signal received, which is the contract -- a second SIGTERM from the
// user is the user's decision, not camp's.
func (r *request) forward(signal syscall.Signal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = true
	askEveryone(signal)
}

// once sends SIGTERM to the namespace unless a forwarded signal already
// did, and reports whether it sent.
func (r *request) once() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sent {
		return false
	}
	r.sent = true
	askEveryone(unix.SIGTERM)
	return true
}

// alreadySent reports whether the namespace has been asked, for the
// message that says so.
func (r *request) alreadySent() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent
}

// askEveryone is the fan-out: one signal to every process in the pid
// namespace, and SIGCONT behind it so a stopped one can act. Every path
// that ends a session goes through here, and this is the only place
// kill(-1) is called -- guarded by Inside's check that this process is
// pid 1, without which it would reach everything the user owns.
func askEveryone(signal syscall.Signal) {
	_ = unix.Kill(-1, signal)
	_ = unix.Kill(-1, unix.SIGCONT)
}

// reapUntil reaps children until the workload is among them, and returns
// its status.
//
// ECHILD before that cannot happen -- the workload is this process's
// child and nothing else reaps it -- and is read as the workload gone
// with a status nobody saw, which is 1, the same answer the launcher gives
// a pipe that closes without one.
func reapUntil(workload int) int {
	for {
		var wait unix.WaitStatus
		pid, err := unix.Wait4(-1, &wait, 0, nil)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case err != nil:
			return 1
		}
		if pid == workload {
			return exitStatus(wait)
		}
	}
}

// ending is what the init does once its workload has exited and the
// end-of-session pass has run: the rest of the namespace is looked at,
// named, asked to leave, and given Grace.
type ending struct {
	// what is "the shell" or "the command" -- the workload as the person
	// who started it would call it.
	what string
	// out is the init's stderr: the terminal while a launcher is attached,
	// and the session's log either way, so the deadline message survives a
	// launcher that was gone by then.
	out io.Writer
	// asked is the session's one request, shared with the signal handler.
	asked *request
}

// poll is how often the ending looks again while it waits. Polling was
// chosen over pidfd readiness on purpose: a 50 ms loop bounded at Grace
// is a page less code in the most sensitive function camp has, and
// nothing about it is observable from outside.
const poll = 50 * time.Millisecond

// run is the sequence, in the order that makes it honest: look before
// asking, say what is being ended before ending it, ask once, wait, and
// at the deadline say what is left and hand it to the kernel.
func (e ending) run() {
	// Look first: a shell that exits cleanly must not produce a paragraph.
	// Children that have already exited are reaped before the look, so a
	// zombie is not mistaken for something still running.
	reapExited()
	left, err := nsx.Processes()
	if err == nil && len(left) == 0 {
		return
	}

	// Not knowing is not an empty namespace. A /proc camp cannot read
	// means camp does not know what is inside, and what it does not know
	// about is still asked and still given the grace -- the alternative is
	// the kernel's SIGKILL with no request before it.
	seconds := int(Grace / time.Second)
	switch {
	case err != nil:
		fmt.Fprintf(e.out, "%s has exited, and camp cannot list what else is in "+
			"this session: %v.\nNot knowing is not an empty session: whatever is "+
			"inside is asked to end (SIGTERM, with SIGCONT so a stopped one can act "+
			"on it), and camp waits up to %d seconds for it, and sends nothing "+
			"stronger.\n", e.what, err, seconds)
	case e.asked.alreadySent():
		fmt.Fprintf(e.out, "%s has exited, and %d process(es) started in this "+
			"session are still running. They were already sent SIGTERM (with SIGCONT "+
			"so a stopped one can act on it) when this session was signalled, and "+
			"are not sent it again:\n%scamp waits up to %d seconds for them, and "+
			"sends nothing stronger.\n", e.what, len(left), listed(left), seconds)
	default:
		fmt.Fprintf(e.out, "%s has exited, and %d process(es) started in this "+
			"session are still running. They are being asked to end (SIGTERM, with "+
			"SIGCONT so a stopped one can act on it):\n%scamp waits up to %d seconds "+
			"for them, and sends nothing stronger.\n", e.what, len(left), listed(left), seconds)
	}
	e.asked.once()

	// Empty is two facts, not one: no child is left to reap, and the
	// namespace's /proc lists nobody but this process. The second is what
	// sees a process that is in the pid namespace without being this
	// process's child -- one that joined the session from outside -- which
	// wait4 never reports, so an init that ended on ECHILD alone would exit
	// under it without ever having asked. A /proc that cannot be read
	// leaves the second fact unknown, and the wait runs to the deadline.
	deadline := time.Now().Add(Grace)
	for {
		noChildren := reapExited()
		left, err = nsx.Processes()
		if err == nil && noChildren && len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(poll)
	}

	// camp does not kill them. The kernel ends every process left in a pid
	// namespace whose first process has exited, with SIGKILL and without
	// appeal (pid_namespaces(7)); that is what exiting now means, and it is
	// said rather than done quietly.
	if err != nil {
		fmt.Fprintf(e.out, "after %d seconds, camp still cannot list what is in "+
			"this session: %v.\ncamp kills nothing. The session's init exits now, "+
			"and the kernel ends every process left in a pid namespace whose first "+
			"process has exited, with SIGKILL.\n", seconds, err)
		return
	}
	fmt.Fprintf(e.out, "after %d seconds, %d process(es) are still in the "+
		"session:\n%scamp does not kill them. The session's init exits now, and "+
		"the kernel ends every process left in a pid namespace whose first "+
		"process has exited, with SIGKILL. Whatever these had not saved is lost. "+
		"If one needed longer to stop, that is the program to look at.\n",
		seconds, len(left), listed(left))
}

// reapExited reaps every child that has already exited, without waiting
// for any that has not, and reports whether no child is left at all.
func reapExited() bool {
	for {
		var wait unix.WaitStatus
		pid, err := unix.Wait4(-1, &wait, unix.WNOHANG, nil)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ECHILD):
			return true
		case err != nil, pid == 0:
			return false
		}
	}
}

// listed renders the processes one per line, by the namespace's own pids
// -- which is what ps inside shows, and nobody is asked to act on them.
func listed(processes []nsx.Process) string {
	var b strings.Builder
	for _, process := range processes {
		fmt.Fprintf(&b, "  pid %d: %s\n", process.PID, process.Command)
	}
	return b.String()
}

func exitStatus(wait unix.WaitStatus) int {
	if wait.Signaled() {
		return 128 + int(wait.Signal())
	}
	return wait.ExitStatus()
}
