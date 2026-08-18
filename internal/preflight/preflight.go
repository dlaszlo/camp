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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/capsx"
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
	checks := []Check{platform(), procfs(), overlayfs(), mountAPI(), git()}
	switch mode {
	case Namespace:
		checks = append(checks, userNamespaces())
	case Privileged:
		// No check for mount(8) or umount(8): camp does not use them. The
		// privileged helper calls mount(2) directly, the same way the
		// namespace mode does, because the messages the binaries print are
		// translated and their exit codes say less than the syscall's errno.
		checks = append(checks, privilege())
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

// mountAPI asks whether this kernel has the mount API camp mounts the
// composed tree with.
//
// fsopen, fsconfig, fsmount and move_mount are how the overlay's layers
// reach the kernel as descriptors rather than as names it resolves itself
// -- which is what makes the directory camp checked and the directory
// camp mounted the same one. There is no fallback: the option-string form
// would silently give up that guarantee, and the /proc/self/fd spelling
// of it records those descriptor paths in the kernel's table for the life
// of the mount, where nothing afterwards can read what was mounted
// (measured).
//
// Asked by opening a context and closing it again, which allocates
// nothing and mounts nothing.
func mountAPI() Check {
	fd, err := unix.Fsopen("overlay", unix.FSOPEN_CLOEXEC)
	switch {
	case err == nil:
		unix.Close(fd)
		return Check{
			Name:   "mount API",
			OK:     true,
			Detail: "fsopen, fsconfig, fsmount and move_mount are available",
			Fatal:  true,
		}
	case errors.Is(err, unix.EPERM):
		// The syscall is there and answered; making an overlay context needs
		// the mount capability, which camp has inside its own namespace and
		// this probe does not. That is the ordinary answer on a working
		// machine, and reading it as a failure would fail every host camp
		// runs on.
		return Check{
			Name: "mount API",
			OK:   true,
			Detail: "present; creating a filesystem needs the mount capability, " +
				"which camp has inside its namespace",
			Fatal: true,
		}
	case errors.Is(err, unix.ENOSYS):
		return Check{
			Name:   "mount API",
			Detail: "this kernel has no fsopen(2)",
			Fatal:  true,
			Hint: "camp gives the overlay its layers as descriptors, through " +
				"fsopen and fsconfig, so that nothing can redirect a layer " +
				"between the check and the mount. The calls have been in Linux " +
				"since 5.2 and overlayfs has taken its layers this way since 6.7. " +
				"There is no fallback: the option-string form would give up that " +
				"guarantee silently.",
		}
	}
	return Check{
		Name:   "mount API",
		Detail: fmt.Sprintf("fsopen(\"overlay\") answered %v", err),
		Fatal:  true,
		Hint: "camp mounts the composed tree through fsopen and fsconfig. A " +
			"container or a sandbox filtering syscalls can hide them, and a " +
			"kernel without the overlay filesystem answers here as well as at " +
			"the check above.",
	}
}

// git is a warning rather than a requirement.
//
// camp uses git for the shipped generation step and for the scans it runs
// when a session ends. A composition of two directories that are not
// repositories needs none of that and works without it -- so a machine
// with no git is worth mentioning and is not a reason to refuse. The one
// place where it really is required refuses there, with the reason.
func git() Check {
	path, err := exec.LookPath("git")
	if err == nil {
		return Check{Name: "tool: git", OK: true, Detail: path, Fatal: true}
	}
	return Check{
		Name:   "tool: git",
		Detail: "not on PATH",
		Hint: "camp needs git for the shipped git_exclude step and for the " +
			"scans it runs when a session ends. A composition that lists no " +
			"generation step does not need it, and works without it.",
	}
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
//
// The child does one thing: it tries to make mount propagation private
// inside its own namespace, which changes nothing anywhere and is the
// smallest mount the composition would make. Existing was not enough.
// On this machine the restriction on unprivileged user namespaces lets
// the namespace be *created* and then confines the process to a profile
// that denies mounting, so a probe that only checked whether the clone
// succeeded reported "permitted" for a namespace nothing can be built
// in. The probe has to attempt the thing the answer is about.
const ProbeArg = "__probe"

// Probe is the body of that child. It reports through its exit status: 0
// if a mount succeeded inside the namespace, 1 if it was refused.
func Probe() int {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		if profile, readErr := os.ReadFile("/proc/self/attr/apparmor/current"); readErr == nil {
			fmt.Fprintf(os.Stderr, "confined to %s\n", strings.TrimSpace(string(profile)))
		}
		return 1
	}
	return 0
}

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

	created, detail := probeUserNamespace()
	if created {
		return Check{Name: "user namespaces", OK: true,
			Detail: "permitted, and a mount inside one succeeds", Fatal: true}
	}

	// Only now, having established that it does not work, is it worth
	// looking at the switches -- to say which one to change.
	return Check{
		Name:   "user namespaces",
		Detail: detail,
		Fatal:  true,
		Hint:   restrictionHint(detail),
	}
}

// probeUserNamespace starts a child in a new user namespace, with the
// same identity mapping and the same carried capabilities a real session
// would use, and asks it to mount something.
//
// Not by reading the switches that could forbid it. There are at least
// three, they interact, and one of them stays switched on system-wide
// even when a profile grants this particular binary an exception -- a
// check that reads that sysctl and concludes reports a confident FAIL on
// a machine where the thing works perfectly. Attempting it answers for
// whatever combination of kernel, LSM and policy this machine actually
// has, including combinations that did not exist when this was written.
func probeUserNamespace() (bool, string) {
	// The real path, not /proc/self/exe: a process already inside the new
	// namespace is the one doing the execve, and where the namespace is
	// restricted that process is confined to a profile which refuses the
	// magic symlink. Probing through it turns "confined" into "refused to
	// create one", which is a different diagnosis with a different repair.
	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}
	cmd := exec.Command(self, ProbeArg)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1},
		},
		GidMappingsEnableSetgroups: false,
		AmbientCaps:                capsx.ForMounting,
	}
	output, err := cmd.CombinedOutput()
	if err == nil {
		return true, ""
	}
	if strings.Contains(string(output), "unprivileged_userns") {
		return false, "the namespace can be created, but this machine confines " +
			"it to the unprivileged_userns profile, which refuses every mount"
	}
	if len(output) > 0 {
		return false, "refused: " + strings.TrimSpace(strings.ReplaceAll(string(output), "\n", "; "))
	}
	return false, "this kernel refused to create one"
}

// restrictionHint names the switch most likely responsible, for a machine
// where the probe has already failed.
func restrictionHint(detail string) string {
	if strings.Contains(detail, "unprivileged_userns") {
		return "This machine restricts unprivileged user namespaces through " +
			"AppArmor, and grants the permission per binary path. camp ships a " +
			"profile that grants it to one path and nothing else:\n" +
			"  sudo install -m 755 camp /usr/local/bin/camp\n" +
			"  sudo install -m 644 packaging/apparmor/camp /etc/apparmor.d/camp\n" +
			"  sudo apparmor_parser -r /etc/apparmor.d/camp\n" +
			"A copy of the binary anywhere else is not covered by the profile, " +
			"so install it first and run it from there. The system-wide " +
			"restriction stays on, which is the point of doing it this way. " +
			"Ubuntu 23.10 and later are the systems this applies to; most " +
			"distributions permit unprivileged user namespaces and need none " +
			"of it."
	}

	if maximum, ok := readInt("/proc/sys/user/max_user_namespaces"); ok && maximum == 0 {
		return "This kernel allows zero user namespaces per user, so nothing " +
			"can create one. Raising it affects every program on the machine, " +
			"and it does not survive a reboot unless you also write it to " +
			"/etc/sysctl.d:\n" +
			"  sudo sysctl -w user.max_user_namespaces=15000\n" +
			"Or use the system-wide mode instead: 'camp up'."
	}

	if allowed, ok := readInt("/proc/sys/kernel/unprivileged_userns_clone"); ok && allowed == 0 {
		return "This kernel carries the older switch that forbids unprivileged " +
			"user namespaces outright -- some Debian and hardened kernels do. " +
			"There is no per-binary exception for it; turning it on affects " +
			"every program on the machine:\n" +
			"  sudo sysctl -w kernel.unprivileged_userns_clone=1\n" +
			"Or use the system-wide mode instead: 'camp up'."
	}

	if restricted, ok := readInt("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); ok && restricted == 1 {
		return "AppArmor restricts unprivileged user namespaces here. Install " +
			"the profile camp ships (packaging/apparmor/camp) so that this one " +
			"binary may create one -- the profile names the path the binary is " +
			"installed at, and a copy run from anywhere else is not covered by " +
			"it. Or use the system-wide mode: 'camp up'."
	}

	if enforcing, ok := readInt("/sys/fs/selinux/enforce"); ok && enforcing == 1 {
		return "Something denied the namespace and none of the usual switches " +
			"is set. SELinux is enforcing on this machine, so it is the likely " +
			"cause: check 'sudo ausearch -m AVC -ts recent' for a denial naming " +
			"this binary. Or use the system-wide mode: 'camp up'."
	}

	return "Something denied the namespace and none of the switches camp knows " +
		"about is set. Check the kernel log ('sudo dmesg | tail') for a denial. " +
		"The system-wide mode does not need a namespace at all: 'camp up'."
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
