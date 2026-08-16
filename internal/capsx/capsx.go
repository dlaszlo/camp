// Package capsx carries the mount capability exactly as far as it is
// needed, and then proves it is gone.
//
// The problem it solves: mounting needs CAP_SYS_ADMIN, and the tools that
// run inside a composition need to see the real user. Mapping the caller
// to uid 0 inside the namespace gives both -- capabilities survive execve
// for a zero euid -- but then everything inside believes it is root, and
// the tools notice -- Claude Code refuses its permission-skip flag as
// apparent root, npm changes behaviour -- and files created inside would
// be owned by a uid nobody asked for.
//
// So the caller's own uid is mapped to itself, which makes the euid
// non-zero and drops every capability at execve -- and the mount
// capability is carried across that boundary in the *ambient* set
// instead, which survives execve on purpose. It is dropped as soon as the
// mounts are made and verified, before anything else runs. The overlay
// keeps working afterwards because the kernel records the mounter's
// credentials at mount time, not at use time.
//
// "Dropped" is a claim, so this package can also read the sets back: the
// acceptance for the whole arrangement is that nothing is left.
package capsx

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// SysAdmin is the capability that permits mounting -- from root, or from
// a user namespace the process created. File ownership does not enter
// into it.
const SysAdmin = unix.CAP_SYS_ADMIN

// ForMounting is the set the composition needs carried across execve,
// and it is two capabilities rather than one because that is what was
// measured, not what was expected.
//
// CAP_SYS_ADMIN is the documented one and it is enough for bind mounts
// and for tmpfs. It is *not* enough for the overlay: with an own-uid
// mapping -- the whole point of route A -- mount(2) returns EACCES for an
// overlay whose lower, upper and work directories were created by the
// mounting process itself, one moment earlier, and are owned by it with
// mode 0755. Adding CAP_DAC_OVERRIDE makes it succeed, and nothing else
// does. Measured by bisecting the ambient set: SYS_ADMIN alone fails,
// SYS_ADMIN with DAC_OVERRIDE succeeds, and the six further capabilities
// tried after it change nothing.
//
// This does not widen what a session can do. Both are dropped before the
// workload is started, and while camp holds them it is the user acting on
// the user's own directories.
var ForMounting = []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_DAC_OVERRIDE}

// Sets is the whole capability state of a process, as /proc reports it.
type Sets struct {
	Inheritable uint64
	Permitted   uint64
	Effective   uint64
	Bounding    uint64
	Ambient     uint64
}

// Empty reports whether nothing is left that could still act.
//
// The bounding set is deliberately not part of this: it only limits what
// could ever be *gained*, and dropping it needs CAP_SETPCAP, which a
// process that has already given everything away no longer has. What
// matters is that nothing is permitted, effective, inheritable or
// ambient.
func (s Sets) Empty() bool {
	return s.Inheritable == 0 && s.Permitted == 0 && s.Effective == 0 && s.Ambient == 0
}

// Has reports whether a capability is in the effective set.
func (s Sets) Has(capability uintptr) bool {
	return s.Effective&(1<<capability) != 0
}

// String renders the sets in the hex /proc uses, for a report.
func (s Sets) String() string {
	return fmt.Sprintf("eff=%016x prm=%016x inh=%016x amb=%016x bnd=%016x",
		s.Effective, s.Permitted, s.Inheritable, s.Ambient, s.Bounding)
}

// Read returns the capability state of a process. Zero means this one.
func Read(pid int) (Sets, error) {
	path := "/proc/self/status"
	if pid > 0 {
		path = fmt.Sprintf("/proc/%d/status", pid)
	}
	file, err := os.Open(path)
	if err != nil {
		return Sets{}, fmt.Errorf("reading the capability state from %s: %w", path, err)
	}
	defer file.Close()

	var sets Sets
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		name, value, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}
		parsed, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
		if err != nil {
			continue
		}
		switch name {
		case "CapInh":
			sets.Inheritable = parsed
		case "CapPrm":
			sets.Permitted = parsed
		case "CapEff":
			sets.Effective = parsed
		case "CapBnd":
			sets.Bounding = parsed
		case "CapAmb":
			sets.Ambient = parsed
		}
	}
	return sets, scanner.Err()
}

// Drop gives away every capability this process holds.
//
// In order, because the order is load-bearing: the ambient set first, so
// that nothing this process starts afterwards inherits anything; then the
// bounding set, while there is still CAP_SETPCAP to do it with; then the
// three ordinary sets, which is the step that cannot be undone.
//
// After this the process can no longer mount anything. That is the point:
// the workload runs with no capability at all, and the mounts it works in
// keep working because the kernel remembers who made them.
func Drop() error {
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("clearing the ambient capability set: %w", err)
	}

	// Best effort: without CAP_SETPCAP this is refused, and the bounding
	// set alone cannot act -- it only bounds what could be gained.
	current, err := Read(0)
	if err == nil && current.Has(unix.CAP_SETPCAP) {
		for capability := 0; capability <= unix.CAP_LAST_CAP; capability++ {
			_ = unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(capability), 0, 0, 0)
		}
	}

	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3, Pid: 0}
	var data [2]unix.CapUserData
	if err := unix.Capset(&header, &data[0]); err != nil {
		return fmt.Errorf("dropping the capability sets: %w", err)
	}
	return nil
}

// Describe renders what a process may still do, for a report that has to
// be readable rather than hexadecimal.
func Describe(sets Sets) string {
	if sets.Empty() {
		return "no capability remains"
	}
	var named []string
	for capability := 0; capability <= unix.CAP_LAST_CAP; capability++ {
		if sets.Effective&(1<<uint(capability)) != 0 {
			named = append(named, Name(uintptr(capability)))
		}
	}
	if len(named) == 0 {
		return "nothing effective, but capabilities remain permitted or " +
			"inheritable: " + sets.String()
	}
	return strings.Join(named, ", ")
}

// Name returns the kernel's name for a capability, for the few camp
// actually talks about.
func Name(capability uintptr) string {
	switch capability {
	case unix.CAP_SYS_ADMIN:
		return "CAP_SYS_ADMIN"
	case unix.CAP_SETPCAP:
		return "CAP_SETPCAP"
	case unix.CAP_SETUID:
		return "CAP_SETUID"
	case unix.CAP_SETGID:
		return "CAP_SETGID"
	case unix.CAP_DAC_OVERRIDE:
		return "CAP_DAC_OVERRIDE"
	default:
		return fmt.Sprintf("cap %d", capability)
	}
}
