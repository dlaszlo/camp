// Package nsx creates the namespace a composition lives in, and decides
// who the processes inside it are.
//
// The namespace mode is primary. Three things follow from building the
// composition inside a user and mount namespace, and together they are
// why it is primary: no privilege is needed, because a process may mount
// inside a user namespace it created itself; nothing leaks, because the
// mounts exist only for processes inside; and teardown cannot fail,
// because when the last process exits the kernel discards the namespace
// and every mount in it -- there is no unmount to refuse and no
// half-removed state to reason about.
//
// Identity is the part that needed a decision. Two routes exist and camp
// takes exactly one of them by itself:
//
// Route A (the default) maps the caller's own uid to itself. Inside, id
// shows the real user, files created are the user's, and the tools that
// check for root see the truth. The cost is that the euid is non-zero, so
// execve drops every capability -- which is why CAP_SYS_ADMIN is carried
// in the ambient set until the mounts are verified and then dropped.
//
// Route B is podman's keep-id shape: newuidmap and newgidmap map the
// caller to itself and hand the subuid range to the rest. It is chosen
// explicitly in the configuration and never engaged by silent fallback,
// because the two routes present different uid worlds to whatever runs
// inside and a quiet switch between them would change what a session's
// files are owned by.
//
// A note the kernel makes necessary: setgroups is denied inside the
// namespace on route A, so supplementary groups cannot be *changed* and
// display as nogroup -- but the kernel credential retains them, and
// permission checks against host files keep honouring them. Measured:
// inside the namespace docker's group-owned socket still works and a
// group-readable log still reads. That is what makes the pre-push gate
// run inside the primary mode.
package nsx

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/capsx"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/refusal"
)

// Identity is how the caller will be mapped inside the namespace.
type Identity struct {
	Route config.Identity
	// InsideUID and InsideGID are what the caller becomes. On route A they
	// are the caller's own ids -- the whole point -- and the fields exist
	// so that a measurement can reproduce the non-zero-euid condition from
	// inside a lab namespace where the caller is already uid 0.
	InsideUID int
	InsideGID int
	// OutsideUID and OutsideGID are the ids being mapped from.
	OutsideUID int
	OutsideGID int
}

// Own returns route A for the calling user: own uid to itself.
func Own() Identity {
	return Identity{
		Route:      config.Ambient,
		InsideUID:  os.Getuid(),
		InsideGID:  os.Getgid(),
		OutsideUID: os.Getuid(),
		OutsideGID: os.Getgid(),
	}
}

// KeepID returns route B's shape: the caller maps to itself, and 0 and
// the rest of the range come from subuid and subgid.
func KeepID() Identity {
	identity := Own()
	identity.Route = config.UIDMap
	return identity
}

// For returns the identity a configuration asks for.
func For(route config.Identity) Identity {
	if route == config.UIDMap {
		return KeepID()
	}
	return Own()
}

// Describe says in one sentence who the caller will be inside.
func (i Identity) Describe() string {
	switch i.Route {
	case config.UIDMap:
		return fmt.Sprintf("uid %d and gid %d map to themselves through "+
			"newuidmap; 0 and the rest of the range come from the subuid range",
			i.InsideUID, i.InsideGID)
	default:
		return fmt.Sprintf("uid %d and gid %d map to themselves; the mount "+
			"capability is carried in the ambient set and dropped before "+
			"anything runs", i.InsideUID, i.InsideGID)
	}
}

// Attrs returns the process attributes that create the namespace.
//
// CLONE_NEWUSER for the capability, CLONE_NEWNS so the mounts are the
// namespace's own, and CLONE_NEWPID so that camp really is pid 1 in it --
// which is what makes it the parent every daemonised process reparents
// to, and therefore what makes the locks last exactly as long as the
// composition.
func (i Identity) Attrs() (*syscall.SysProcAttr, error) {
	attrs := &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID,
		// The mount capabilities, carried across execve on purpose and
		// given back as soon as the mounts are verified.
		AmbientCaps: capsx.ForMounting,
	}

	switch i.Route {
	case config.UIDMap:
		if err := haveIDMapTools(); err != nil {
			return nil, err
		}
		// The maps are written from outside by newuidmap once the child
		// exists, so none are declared here.
		return attrs, nil

	default:
		attrs.UidMappings = []syscall.SysProcIDMap{
			{ContainerID: i.InsideUID, HostID: i.OutsideUID, Size: 1},
		}
		attrs.GidMappings = []syscall.SysProcIDMap{
			{ContainerID: i.InsideGID, HostID: i.OutsideGID, Size: 1},
		}
		// Writing a gid map without newgidmap requires denying setgroups
		// first: the kernel insists, to stop a process dropping a group in
		// order to gain access it was being denied by it. Supplementary
		// groups are retained either way, and keep granting access to host
		// files.
		attrs.GidMappingsEnableSetgroups = false
		return attrs, nil
	}
}

// haveIDMapTools reports whether route B can run at all.
//
// It refuses rather than falling back to route A. The two routes present
// different uid worlds to whatever runs inside, and a silent switch
// between them would change what a session's files end up owned by --
// which is exactly the class of surprise camp exists to remove.
func haveIDMapTools() error {
	var missing []string
	for _, tool := range []string{"newuidmap", "newgidmap"} {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return refusal.New("identity-uidmap-missing",
		"the configuration asks for 'identity: uidmap', and %v is not "+
			"installed on this machine.\n"+
			"That route maps your uid to itself and hands 0 and the rest of the "+
			"range to your subuid range, the way rootless podman does, and it "+
			"needs the uidmap package to write those maps:\n"+
			"  sudo apt install uidmap\n"+
			"camp will not quietly use the other route instead: the two present "+
			"different uid worlds to whatever runs inside, and files created in "+
			"a session would end up owned by different ids depending on which "+
			"one ran.", missing)
}

// WriteMaps writes route B's maps for a child that is waiting for them.
//
// Route A needs none of this -- the clone declares its single mapping and
// the kernel writes it. Route B has to run outside the namespace, after
// the child exists, because newuidmap is what holds the privilege to map
// a whole range.
func (i Identity) WriteMaps(pid int) error {
	if i.Route != config.UIDMap {
		return nil
	}
	if err := runIDMap("newuidmap", pid, i.InsideUID, i.OutsideUID); err != nil {
		return err
	}
	return runIDMap("newgidmap", pid, i.InsideGID, i.OutsideGID)
}

// runIDMap maps the caller to itself and gives the subuid range to
// everything else, which is podman's keep-id shape.
func runIDMap(tool string, pid, inside, outside int) error {
	base, count, err := subordinateRange(tool)
	if err != nil {
		return err
	}
	// <inside> <outside> 1  : the caller, to itself.
	// 0 <base> <inside>     : everything below the caller, from the range.
	// <inside+1> <base+inside> <count-inside> : everything above it.
	arguments := []string{
		fmt.Sprint(pid),
		fmt.Sprint(inside), fmt.Sprint(outside), "1",
		"0", fmt.Sprint(base), fmt.Sprint(inside),
	}
	if count > inside+1 {
		arguments = append(arguments,
			fmt.Sprint(inside+1), fmt.Sprint(base+inside), fmt.Sprint(count-inside-1))
	}
	command := exec.Command(tool, arguments...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	if output, err := command.CombinedOutput(); err != nil {
		return refusal.New("identity-uidmap-failed",
			"%s refused to write the map for the namespace: %v\n%s\n"+
				"Check that /etc/subuid and /etc/subgid give your account a range.",
			tool, err, output)
	}
	return nil
}

// subordinateRange reads the range this account was given.
func subordinateRange(tool string) (int, int, error) {
	file := "/etc/subuid"
	name := currentUserName()
	if tool == "newgidmap" {
		file = "/etc/subgid"
	}
	base, count, err := readSubordinate(file, name)
	if err != nil {
		return 0, 0, refusal.New("identity-subrange-missing",
			"%s gives no range to %q: %v.\n"+
				"Route B maps 0 and everything except your own id out of that "+
				"range. Add a line, for example:\n  %s:100000:65536",
			file, name, err, name)
	}
	return base, count, nil
}

func currentUserName() string {
	if name := os.Getenv("USER"); name != "" {
		return name
	}
	return fmt.Sprint(os.Getuid())
}

// MountProc gives the namespace its own view of processes.
//
// Without it /proc still shows the outer namespace's pids, and every
// answer camp gives about what is holding what -- the holder report, ps,
// the reaping -- would be about processes that do not exist in here.
func MountProc() error {
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	if err := unix.Mount("proc", "/proc", "proc", flags, ""); err != nil {
		return fmt.Errorf("mounting a private /proc for the namespace: %w", err)
	}
	return nil
}

// Detach stops every mount made here from travelling back out.
//
// Mounts propagate by default on a systemd machine, and without this the
// isolation the whole mode rests on would be an illusion: a mount made
// inside would appear outside, on the backing store's own path.
func Detach() error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("making mount propagation private: %w", err)
	}
	return nil
}

// Report is who the process turned out to be inside the namespace.
type Report struct {
	UID    int
	GID    int
	Groups []int
	Caps   capsx.Sets
}

// Look reports the identity of the calling process.
func Look() Report {
	groups, _ := os.Getgroups()
	sets, _ := capsx.Read(0)
	return Report{UID: os.Getuid(), GID: os.Getgid(), Groups: groups, Caps: sets}
}

// String renders the report for a person.
func (r Report) String() string {
	return fmt.Sprintf("uid=%d gid=%d groups=%v; %s",
		r.UID, r.GID, r.Groups, capsx.Describe(r.Caps))
}
