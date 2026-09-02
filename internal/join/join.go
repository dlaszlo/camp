// Package join finds a running session's init and opens the namespace
// descriptors a second terminal enters it through.
//
// It decides, and nothing else acts here: cli execs util-linux nsenter
// with the descriptors this package opens, and session runs the joined
// process (JoinedMain) on the other side. The split is the spec's
// one-package-one-responsibility rule -- discovery is a question, joining
// is an action, and they answer to different failures.
//
// Everything about a session is read from /proc, because a session leaves
// no record by design (C21). The init is a process: its command line names
// its configuration, its /proc/<pid>/status says whose it is and that it
// is pid 1 of its own namespace, its /proc/<pid>/mountinfo -- the mount
// table of its own mount namespace, readable from outside for a same-uid
// process -- says where it composes, and its open descriptors say on which
// directories it holds the locks. Argv can be forged by any process and a
// pathname can be renamed under, but a descriptor to an inode cannot, so
// the descriptors are the decisive check.
//
// Nothing here parses a program's output. Every fact is a file under /proc,
// and the one external tool (nsenter) is given descriptors and never
// consulted for anything but its exit status.
package join

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/enc"
	"github.com/dlaszlo/camp/internal/locks"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/nsx"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/refusal"
)

// Target is the running session a join will enter: its init and the three
// namespace files nsenter joins through.
//
// The namespace files are open descriptors, opened once after every check
// has passed, so that the pid being reused between the check and the join
// cannot redirect the join: nsenter is handed the files, not the pid.
type Target struct {
	// PID is the init, the session's pid 1, for the messages.
	PID int
	// Live is the composed tree its mount table shows, for the report.
	Live string

	user, mnt, pid *os.File
	pidfd          int
}

// Files are the three namespace descriptors, in the order nsenter is told
// about them: user, mount, pid.
func (t *Target) Files() []*os.File { return []*os.File{t.user, t.mnt, t.pid} }

// CloseFiles releases the three namespace descriptors and keeps the pidfd.
//
// The caller closes them the moment the child has its copies. An open
// descriptor to a mount namespace keeps that namespace -- and the overlay
// in it -- alive after every process has left it, so a joiner still
// holding one when the session ended would hold the old overlay open on an
// upper whose lock the init has already released; a new start could then
// mount a second overlay on the same upper, which is the corruption the
// locks exist to prevent (C8). The pidfd holds no namespace, only the
// identity of the init, and stays for the liveness question afterwards.
func (t *Target) CloseFiles() {
	for _, file := range []**os.File{&t.user, &t.mnt, &t.pid} {
		if *file != nil {
			(*file).Close()
			*file = nil
		}
	}
}

// Ended reports whether the init has exited, waiting up to the given time
// for it to.
//
// The wait exists for one ordering the kernel imposes: when the init
// exits, the kernel ends every other process in the pid namespace and
// waits for them to be reaped before the init's own exit completes. A
// joined process's death is therefore observed -- by nsenter, and then by
// the joiner -- while the init is still on its way out, so a joiner that
// asked at once whether the init was gone would be told no, and say
// nothing about a session that had ended. A short bound covers that
// window; a joined process that simply exited is answered without waiting.
func (t *Target) Ended(within time.Duration) bool { return ended(t.pidfd, within) }

// ended asks a pidfd whether its process has exited: a pidfd becomes
// readable the moment the process exits, before anyone has reaped it.
//
// Not a signal-0 probe. That answers "may I signal this pid", which is yes
// for a zombie -- and the init is a zombie from its exit until its launcher
// reaps it, which is exactly the window in which a joined shell has just
// been ended by the kernel and the joiner asks why. A pidfd names that one
// process, so a reused pid cannot answer for it either way.
func ended(pidfd int, within time.Duration) bool {
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	deadline := time.Now().Add(within)
	for {
		left := max(int(time.Until(deadline)/time.Millisecond), 0)
		n, err := unix.Poll(fds, left)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err == nil && n > 0
	}
}

// Close releases everything the target holds.
func (t *Target) Close() {
	t.CloseFiles()
	if t.pidfd >= 0 {
		unix.Close(t.pidfd)
		t.pidfd = -1
	}
}

// candidate is one init that names this configuration, held by a pidfd
// and sorted into what it turned out to be when its /proc was read.
type candidate struct {
	pid   int
	pidfd int
	uid   int
	// live is the overlay's mount point in the init's own table, empty when
	// it has none at the configuration's live path.
	live string
	// locked reports that the init holds descriptors to the directories now
	// standing at the configuration's upper and live paths.
	locked bool
}

func (c candidate) close() { unix.Close(c.pidfd) }

// Find locates the one running session of this configuration, or says why
// it cannot.
//
// It reads /proc rather than any record, and the process list is every
// process because the init could be anywhere in the tree: its launcher is
// usually long gone and it has reparented to pid 1.
func Find(cfg config.Config) (*Target, refusal.List) {
	var refused refusal.List

	// If pid 1 of this pid namespace is this configuration's own init, the
	// shell this was typed into is already in the session it means to join.
	// Decided from /proc/1 before the scan: you cannot join what you are in.
	position := standing(cfg)
	if position == insideThis {
		refused.Add("join-from-inside",
			"pid 1 of this pid namespace is camp's session init for %s -- the "+
				"configuration this command names -- so the shell this was typed "+
				"into is already in that session.\n"+
				"You are already in it: work here. To open another view of the same "+
				"tree, run '%s --join' from a terminal that is not inside a "+
				"session.", cfg.Source, shellCommand(cfg))
		return nil, refused
	}

	mine, err := pathx.Real(cfg.Source)
	if err != nil {
		refused.Add("join-no-session",
			"the configuration %s could not be resolved: %v.\n"+
				"camp finds a running session by matching each init's configuration "+
				"against this one, and it cannot do that without resolving this one. "+
				"Check the path.", cfg.Source, err)
		return nil, refused
	}

	// The two directories the configuration names now, as inodes: what a
	// candidate has to hold the locks on to be this composition.
	upperNow, liveNow, err := lockedDirectories(cfg)
	if err != nil {
		refused.Add("join-no-session",
			"the directories %s names could not be looked at: %v.\n"+
				"A running session is recognised by the locks its init holds on the "+
				"code repository and the composed tree's directory, and camp cannot "+
				"compare against directories it cannot see. Check the paths.",
			cfg.Source, err)
		return nil, refused
	}

	var passing, otherUser, notThis []candidate
	var unreadable []string
	for _, pid := range initPids(mine) {
		info, err := inspect(pid, cfg, upperNow, liveNow)
		switch {
		case errors.Is(err, errGone):
			continue // exited between the scan and the read: not a candidate
		case err != nil:
			// Not gone and not readable is not "no session": it is camp not
			// knowing, and it is said rather than counted as nothing.
			unreadable = append(unreadable, fmt.Sprintf("pid %d: %v", pid, err))
			continue
		}
		switch {
		case info.uid != os.Getuid():
			otherUser = append(otherUser, info)
		case info.live == cfg.Live() && info.locked:
			passing = append(passing, info)
		default:
			notThis = append(notThis, info)
		}
	}
	defer closeAll(otherUser, notThis)

	switch {
	case len(passing) == 1:
		target, refusedOpen := open(passing[0], cfg)
		return target, refusedOpen
	case len(passing) > 1:
		closeAll(passing)
		refused.Add("join-ambiguous",
			"more than one running session names %s and passes every check, and "+
				"camp will not pick one:\n%s"+
				"This should not be reachable -- two inits cannot hold the locks on "+
				"one upper at once -- so end all but the one you mean (each with "+
				"'kill -TERM <pid>', which ends that session), and join again.",
			cfg.Source, listed(passing))
		return nil, refused
	case len(unreadable) > 0:
		refused.Add("join-init-unreadable",
			"a process names itself camp's init for %s and camp could not read "+
				"what it is:\n  %s\n"+
				"camp will not join a session it cannot verify, and will not say "+
				"there is none. Look at that process; if it is a session of yours, "+
				"the usual cause is a descriptor limit in this shell (ulimit -n).",
			cfg.Source, strings.Join(unreadable, "\n  "))
		return nil, refused
	case len(otherUser) > 0:
		refused.Add("join-other-user",
			"a session for %s is running as uid %d, and a session can be entered "+
				"only by the user who started it.\n"+
				"Joining its namespaces needs the capabilities that user has in them "+
				"and nobody else does. Ask that user to work in it, or start your own "+
				"session on a composition of your own.",
			cfg.Source, otherUser[0].uid)
		return nil, refused
	case len(notThis) > 0:
		it := notThis[0]
		refused.Add("join-not-this-composition",
			"an init (pid %d) names %s but the session it is running is not the "+
				"composition that file now describes: %s.\n"+
				"The configuration was read as it was when the session started; the "+
				"file now names the composed tree %s over %s. The running session is "+
				"what is true inside it; the file is what the next start would build. "+
				"Join nothing here -- end that session ('kill -TERM %d') and start "+
				"again with '%s', or put the file back.",
			it.pid, cfg.Source, describe(it, cfg), cfg.Live(), cfg.UpperPath(),
			it.pid, shellCommand(cfg))
		return nil, refused
	case position == insideAnother:
		// Nothing passing was found, and pid 1 here is a camp init for
		// another configuration: this command is inside a different session,
		// whose pid namespace shows only its own processes. A session nested
		// inside this one is visible and would have been found; a sibling
		// session's init is not among them, which is why the one asked for
		// was not -- said as its own refusal rather than folded into "no
		// session", because the repair is different.
		refused.Add("join-from-another-session",
			"no session for %s was found, and pid 1 of this pid namespace is "+
				"camp's session init for another configuration -- so this command "+
				"was started from inside a different session.\n"+
				"Only that session's own processes are visible from here, so a "+
				"sibling session's init cannot be seen whether or not it runs. Run "+
				"'%s --join' from a terminal that is not inside a session.",
			cfg.Source, shellCommand(cfg))
		return nil, refused
	default:
		refused.Add("join-no-session",
			"no session is running for %s.\n"+
				"camp looked for its init among the processes visible from here -- "+
				"camp's init is pid 1 of a session's own pid namespace and names its "+
				"configuration on its command line -- and found none. Start one: "+
				"'%s'. A session that has just ended shows as nothing here, and that "+
				"is correct: it left nothing behind.", cfg.Source, shellCommand(cfg))
		return nil, refused
	}
}

// errGone is a candidate that exited between the scan and the read.
var errGone = errors.New("the process is gone")

// inspect holds one candidate init by pidfd and then reads what it is.
//
// The pidfd comes first, and every read comes after it, because a pid can
// be reused: a verified init that exits and has its number taken by another
// same-uid init would otherwise leave camp holding facts about the original
// and descriptors to the replacement. With the pidfd open first, a probe
// through it after the reads proves the reads were about the process the
// pidfd names -- a reused pid leaves the original pidfd stale.
func inspect(pid int, cfg config.Config, upperNow, liveNow pathx.Identity) (candidate, error) {
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return candidate{}, errGone
		}
		return candidate{}, fmt.Errorf("holding it by pidfd: %w", err)
	}
	info := candidate{pid: pid, pidfd: pidfd}

	uid, nspidLast, err := statusFacts(pid)
	if err != nil {
		info.close()
		if errors.Is(err, os.ErrNotExist) {
			return candidate{}, errGone
		}
		return candidate{}, fmt.Errorf("reading its status: %w", err)
	}
	if nspidLast != 1 {
		// Not pid 1 of its innermost namespace, which camp's init refuses to
		// be anything but: whatever this is, it is not a session's init.
		info.close()
		return candidate{}, errGone
	}
	info.uid = uid

	// For an init of another user the reads below are refused, and they are
	// not needed: uid decides that case on its own.
	if uid == os.Getuid() {
		info.live = overlayAt(pid, cfg.Live())
		info.locked = locks.HoldsOpen(pid, upperNow) && locks.HoldsOpen(pid, liveNow)
	}

	// The probe: still the same process, so the facts above are its.
	if ended(pidfd, 0) {
		info.close()
		return candidate{}, errGone
	}
	return info, nil
}

// open opens the chosen init's namespace files and probes the pidfd once
// more afterwards: a failure to open is the session having ended in the
// gap, and a pidfd that no longer answers means the files may belong to a
// process that reused the pid, so they are not used.
func open(it candidate, cfg config.Config) (*Target, refusal.List) {
	var refused refusal.List
	target := &Target{PID: it.pid, Live: it.live, pidfd: it.pidfd}
	for _, ns := range []struct {
		name string
		into **os.File
	}{
		{"user", &target.user},
		{"mnt", &target.mnt},
		{"pid", &target.pid},
	} {
		file, err := os.Open(fmt.Sprintf("/proc/%d/ns/%s", it.pid, ns.name))
		if err != nil {
			target.Close()
			if errors.Is(err, os.ErrNotExist) {
				refused.Add("join-ended",
					"the session for %s ended between camp finding it and camp joining "+
						"it.\nStart one: '%s'.", cfg.Source, shellCommand(cfg))
				return nil, refused
			}
			refused.Add("join-init-unreadable",
				"the %s namespace of the session's init (pid %d) could not be opened: "+
					"%v.\ncamp will not join a session it cannot hold every namespace "+
					"of. The session is running; look at what stops this shell opening "+
					"a file under /proc/%d -- a descriptor limit (ulimit -n) is the "+
					"usual cause.", ns.name, it.pid, err, it.pid)
			return nil, refused
		}
		*ns.into = file
	}
	if target.Ended(0) {
		target.Close()
		refused.Add("join-ended",
			"the session for %s ended between camp finding it and camp joining "+
				"it.\nStart one: '%s'.", cfg.Source, shellCommand(cfg))
		return nil, refused
	}
	return target, nil
}

// lockedDirectories is the code repository and the composed tree's
// directory as they stand now, as the inodes a session's init has to hold
// the locks on -- resolved beneath the environment root, following no
// symlink, the way every operand camp checks is resolved.
func lockedDirectories(cfg config.Config) (upper, live pathx.Identity, err error) {
	repo, ok := cfg.Repository(cfg.Upper)
	if !ok {
		return upper, live, fmt.Errorf("the configuration names no upper")
	}
	upperInfo, err := pathx.StatBeneath(cfg.Env, repo.Path.Components())
	if err != nil {
		return upper, live, err
	}
	liveInfo, err := pathx.StatBeneath(cfg.Env, cfg.Merged.Components())
	if err != nil {
		return upper, live, err
	}
	if !upperInfo.Exists() || !liveInfo.Exists() {
		return upper, live, fmt.Errorf("%s or %s does not exist", cfg.UpperPath(), cfg.Live())
	}
	return upperInfo.Ident, liveInfo.Ident, nil
}

// initPids lists every process whose command line is camp's session init
// naming a configuration that resolves to the same file as mine.
//
// Both sides are resolved, so two spellings of one path agree; a candidate
// whose recorded path no longer resolves is not this configuration's and
// is left out rather than guessed at.
func initPids(mine string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}
		args := splitCmdline(data)
		if len(args) < 3 || args[1] != nsx.InitArg {
			continue
		}
		resolved, err := pathx.Real(args[2])
		if err != nil || resolved != mine {
			continue
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}

// statusFacts reads the real uid and the last NSpid field from a process's
// status.
//
// The real uid is the first of the four on the Uid line; the last NSpid
// field is the process's pid in its innermost namespace, which is 1 for an
// init.
func statusFacts(pid int) (uid, nspidLast int, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, 0, err
	}
	haveUID, haveNSpid := false, false
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "Uid:"):
			fields := strings.Fields(line[len("Uid:"):])
			if len(fields) > 0 {
				if value, err := strconv.Atoi(fields[0]); err == nil {
					uid, haveUID = value, true
				}
			}
		case strings.HasPrefix(line, "NSpid:"):
			fields := strings.Fields(line[len("NSpid:"):])
			if len(fields) > 0 {
				if value, err := strconv.Atoi(fields[len(fields)-1]); err == nil {
					nspidLast, haveNSpid = value, true
				}
			}
		}
	}
	if !haveUID || !haveNSpid {
		return 0, 0, fmt.Errorf("/proc/%d/status has no Uid or NSpid line", pid)
	}
	return uid, nspidLast, nil
}

// overlayAt reads a process's own mount table and reports whether an
// overlay stands at the given live path there, returning its mount point.
//
// The mount table is that process's mount namespace, readable from outside
// for a same-uid process. The mount point is compared by name -- the mount
// itself is by name, as spec §6 says -- and which directories the overlay
// is really on is answered by the init's lock descriptors, not by the
// pathnames in the option string, which keep whatever spelling was given
// at mount time and can be renamed under.
func overlayAt(pid int, live string) string {
	table, err := mountinfo.Read(fmt.Sprintf("/proc/%d/mountinfo", pid))
	if err != nil {
		return ""
	}
	for _, entry := range mountinfo.AllOverlays(table) {
		if entry.Point == live {
			return entry.Point
		}
	}
	return ""
}

// insidePosition is what /proc/1 says about where this command was run.
type insidePosition int

const (
	outsideAny insidePosition = iota
	insideThis
	insideAnother
)

// standing reads /proc/1 and reports whether this command is inside a
// session, and if so whether it is the one it means to join.
func standing(cfg config.Config) insidePosition {
	data, err := os.ReadFile("/proc/1/cmdline")
	if err != nil {
		return outsideAny
	}
	args := splitCmdline(data)
	if len(args) < 3 || args[1] != nsx.InitArg {
		return outsideAny
	}
	mine, err1 := pathx.Real(cfg.Source)
	theirs, err2 := pathx.Real(args[2])
	if err1 == nil && err2 == nil && mine == theirs {
		return insideThis
	}
	return insideAnother
}

// splitCmdline turns a NUL-separated /proc cmdline into its arguments.
func splitCmdline(data []byte) []string {
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
}

// describe says what a candidate's /proc showed, for the
// not-this-composition refusal.
func describe(it candidate, cfg config.Config) string {
	if it.live == "" {
		return fmt.Sprintf("its mount table has no overlay at %s", cfg.Live())
	}
	return fmt.Sprintf("it composes at %s but holds the locks on directories "+
		"other than the ones now at %s and %s", it.live, cfg.UpperPath(), cfg.Live())
}

// listed renders ambiguous candidates one per line, by pid.
func listed(candidates []candidate) string {
	var b strings.Builder
	for _, it := range candidates {
		fmt.Fprintf(&b, "  pid %d: %s\n", it.pid, nsx.Command(it.pid))
	}
	return b.String()
}

func closeAll(lists ...[]candidate) {
	for _, list := range lists {
		for _, it := range list {
			it.close()
		}
	}
}

// shellCommand is the exact command that starts a session of this
// configuration, with the file named: the reader may have reached this
// with -f, and a bare 'camp shell' from their directory could find a
// different configuration or none.
func shellCommand(cfg config.Config) string {
	return "camp shell -f " + enc.Shell(cfg.Source)
}

// JoinArgs is the nsenter argument vector that joins a target and runs
// camp __joined inside it.
//
// The namespace files are named as descriptors -- fds 3, 4, 5, the order
// Files returns them, which is the order the caller passes them as
// ExtraFiles -- so nsenter joins the files and not the pids, and a pid
// reused after the check cannot redirect it. --preserve-credentials keeps
// the caller's uid (route A maps it to itself), and --wd starts the joined
// process in the composed tree. The pid namespace is joined too, so nsenter
// forks the child into it and waits, re-raising a fatal signal on itself so
// the status reaches the caller the way a shell would report it.
//
// The live path travels with the configuration path: the joined process
// stands in the tree discovery verified, and does not re-derive it from a
// file that may have been edited in between.
func JoinArgs(self, configPath, live string, workload []string) []string {
	args := []string{
		"--user=/proc/self/fd/3",
		"--mount=/proc/self/fd/4",
		"--pid=/proc/self/fd/5",
		"--preserve-credentials",
		"--wd=" + live,
		"--", self, nsx.JoinedArg, configPath, live, "--",
	}
	return append(args, workload...)
}

// Nsenter is where nsenter has to be for a join, and the message when it is
// not there.
//
// A join cannot be done in camp's own process: setns(2) into a user
// namespace answers EINVAL for a multithreaded caller, and a Go process is
// multithreaded before main. camp hands the namespace descriptors to
// nsenter, which is single-threaded and does the setns and fork itself.
func Nsenter() (string, refusal.R, bool) {
	path, err := exec.LookPath("nsenter")
	if err != nil {
		return "", refusal.New("join-tool-missing",
			"camp cannot join a session without nsenter, and it is not on PATH.\n"+
				"A join enters the session's namespaces with setns(2), which the "+
				"kernel refuses to a multithreaded process -- and camp is one before "+
				"its own code runs -- so camp hands the namespace descriptors to "+
				"util-linux's nsenter, which is single-threaded. Install it:\n"+
				"  sudo apt install util-linux\n"+
				"It is an essential package on every Debian-derived system, so this "+
				"is unusual."), false
	}
	return path, refusal.R{}, true
}
