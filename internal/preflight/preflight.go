// Package preflight reports what this machine has to provide before any
// of this can work.
//
// Checked up front and named individually, because the failure modes
// otherwise arrive disguised. Without OverlayFS the mount fails with
// "invalid argument". Without /proc the holder report silently finds
// nothing and a busy composition looks idle. Where unprivileged user
// namespaces are restricted, the rootless mode fails while writing a uid
// map, which reads as an internal error and is not one. On macOS or
// Windows the whole approach is unavailable, and the honest thing is to
// say so in one sentence rather than fail somewhere deep in a mount.
package preflight

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Mode is the way a composition is going to be built, because the two
// modes need different things from the machine.
type Mode string

const (
	// Namespace is the rootless mode: mounts inside a user namespace.
	Namespace Mode = "namespace"
	// Privileged is the system-wide mode: mounts visible to every process.
	Privileged Mode = "privileged"
)

// Check is one requirement and whether this machine meets it.
type Check struct {
	Name   string
	OK     bool
	Detail string
	Fatal  bool
	Hint   string
}

// Symbol is the short marker a report puts in front of the line.
func (c Check) Symbol() string {
	switch {
	case c.OK:
		return "ok"
	case c.Fatal:
		return "FAIL"
	default:
		return "warn"
	}
}

// Run evaluates every requirement for a mode. All of them are evaluated,
// so one run reports everything rather than one thing per attempt.
func Run(mode Mode) []Check {
	checks := []Check{platform(), procfs(), overlayfs(), tool("git")}
	switch mode {
	case Namespace:
		checks = append(checks, userNamespaces())
	case Privileged:
		checks = append(checks, tool("mount"), tool("umount"), privilege())
	}
	return checks
}

// Failures returns the checks that are fatal and unmet.
func Failures(checks []Check) []Check {
	var failed []Check
	for _, check := range checks {
		if check.Fatal && !check.OK {
			failed = append(failed, check)
		}
	}
	return failed
}

func platform() Check {
	if runtime.GOOS == "linux" {
		return Check{Name: "platform", OK: true, Detail: runtime.GOOS + "/" + runtime.GOARCH, Fatal: true}
	}
	return Check{
		Name:   "platform",
		Detail: runtime.GOOS + " is not Linux",
		Fatal:  true,
		Hint: "camp composes directories with OverlayFS, a Linux kernel " +
			"filesystem. On macOS or Windows the nearest equivalent is to run " +
			"the composition inside a Linux VM or container.",
	}
}

func procfs() Check {
	if _, err := os.Stat("/proc/self/mountinfo"); err == nil {
		return Check{Name: "/proc", OK: true, Detail: "readable", Fatal: true}
	}
	return Check{
		Name:   "/proc",
		Detail: "not available",
		Fatal:  true,
		Hint: "camp reads /proc to see what is mounted and to name the processes " +
			"holding a composition. Without it neither is possible.",
	}
}

func overlayfs() Check {
	data, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return Check{
			Name:   "overlayfs",
			Detail: "could not read /proc/filesystems",
			Fatal:  true,
			Hint:   "check that /proc is mounted",
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "nodev\toverlay" ||
			strings.HasSuffix(strings.TrimSpace(line), "overlay") {
			return Check{
				Name:   "overlayfs",
				OK:     true,
				Detail: "supported by the running kernel",
				Fatal:  true,
			}
		}
	}
	return Check{
		Name:   "overlayfs",
		Detail: "the running kernel does not list overlay in /proc/filesystems",
		Fatal:  true,
		Hint:   "try 'sudo modprobe overlay', or use a kernel built with it",
	}
}

func tool(name string) Check {
	path, err := exec.LookPath(name)
	if err != nil {
		return Check{
			Name:   "tool: " + name,
			Detail: "not on PATH",
			Fatal:  true,
			Hint:   "install the package providing " + name,
		}
	}
	return Check{Name: "tool: " + name, OK: true, Detail: path, Fatal: true}
}

func privilege() Check {
	if os.Geteuid() == 0 {
		return Check{Name: "privilege", OK: true, Detail: "running as root", Fatal: true}
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return Check{
			Name:   "privilege",
			Detail: "not root and sudo is not installed",
			Fatal:  true,
			Hint:   "mounting needs root here; install sudo, run as root, or use 'camp run'",
		}
	}
	return Check{
		Name:   "privilege",
		OK:     true,
		Detail: "not root; sudo will be used and may ask for a password",
		Fatal:  false,
	}
}

// ProbeArg is the hidden argument the capability probe is started with.
// The child does nothing but exist and exit.
const ProbeArg = "__probe"

// userNamespaces reports whether this process may create a user namespace,
// by creating one.
//
// Not by reading the switches that could forbid it. There are at least
// three, they interact, and one of them -- the AppArmor restriction on
// Ubuntu 23.10 and later -- stays switched on system-wide even when a
// profile grants this particular binary an exception. A check that reads
// that sysctl and concludes reports a confident FAIL on a machine where
// the thing works perfectly.
//
// So it is attempted instead. The answer is then true for whatever
// combination of kernel, LSM, container runtime and policy this machine
// actually has, including combinations that did not exist when this was
// written.
func userNamespaces() Check {
	if os.Geteuid() == 0 {
		return Check{Name: "user namespaces", OK: true, Detail: "running as root", Fatal: true}
	}

	if err := probeUserNamespace(); err == nil {
		return Check{Name: "user namespaces", OK: true, Detail: "permitted", Fatal: true}
	}

	// Only now, having established that it does not work, is it worth
	// looking at the switches -- to say which one to change.
	return Check{
		Name:   "user namespaces",
		Detail: "this kernel refused to create one",
		Fatal:  true,
		Hint:   restrictionHint(),
	}
}

// probeUserNamespace starts a child in a new user namespace and waits for
// it. The child is this same binary, which exits immediately.
func probeUserNamespace() error {
	cmd := exec.Command("/proc/self/exe", ProbeArg)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
		GidMappingsEnableSetgroups: false,
	}
	return cmd.Run()
}

// restrictionHint names the switch most likely responsible, for a machine
// where the probe has already failed.
func restrictionHint() string {
	if maximum, ok := readInt("/proc/sys/user/max_user_namespaces"); ok && maximum == 0 {
		return "sudo sysctl -w user.max_user_namespaces=15000"
	}
	if allowed, ok := readInt("/proc/sys/kernel/unprivileged_userns_clone"); ok && allowed == 0 {
		return "sudo sysctl -w kernel.unprivileged_userns_clone=1"
	}
	if restricted, ok := readInt("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); ok && restricted == 1 {
		return "AppArmor restricts unprivileged user namespaces on this system. " +
			"Install the profile shipped with camp (packaging/apparmor/camp) so " +
			"that this one binary may create one -- note that the profile has to " +
			"name the path the binary is actually installed at, and that a copy " +
			"run from anywhere else is not covered by it. Or use the privileged " +
			"mode: 'camp up'. Turning the restriction off system-wide works too, " +
			"but removes a protection from every program on the machine."
	}
	return "run this again with 'camp doctor' after checking dmesg: something " +
		"denied the namespace without one of the usual switches being set"
}

func readInt(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return value, true
}
