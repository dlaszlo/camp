package nsx_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/capsx"
	"github.com/dlaszlo/camp/internal/mountx"
	"github.com/dlaszlo/camp/internal/nsx"
	"github.com/dlaszlo/camp/internal/testenv"
)

// The identity spike: route A, measured rather than argued.
//
// The claim being tested is a chain, and every link of it is load-bearing.
// The caller's own uid maps to itself, so the euid inside is not zero and
// execve therefore drops every capability -- which is why CAP_SYS_ADMIN
// is carried in the ambient set. With it the mounts succeed. The ambient
// set is then dropped, and the overlay keeps working anyway, because the
// kernel recorded the mounter's credentials at mount time. And the
// supplementary groups the caller had outside still grant access to host
// files inside, even though setgroups is denied and they display as
// nogroup -- which is what makes the repository's own pre-push gate run
// in this mode.
//
// Running it needs permission to create a user namespace. On this machine
// that permission is granted by an AppArmor profile to one installed
// binary path, so a freshly built test binary cannot open a namespace
// until camp and its profile are installed. The test says so and
// skips rather than pretending.

const (
	insideEnv = "CAMP_SPIKE_INSIDE"
	rootEnv   = "CAMP_SPIKE_ROOT"
	uidEnv    = "CAMP_SPIKE_INSIDE_UID"
	gidEnv    = "CAMP_SPIKE_INSIDE_GID"
)

func TestMain(m *testing.M) {
	if os.Getenv(insideEnv) != "" {
		os.Exit(inside())
	}
	os.Exit(m.Run())
}

// result is what the process inside the namespace reports back.
type result struct {
	UID    int   `json:"uid"`
	GID    int   `json:"gid"`
	Groups []int `json:"groups"`

	CapsBeforeDrop string `json:"caps_before_drop"`
	CapsAfterDrop  string `json:"caps_after_drop"`
	CapsEmptyAfter bool   `json:"caps_empty_after"`

	Mounted               bool   `json:"mounted"`
	IslandWrite           string `json:"island_write"`
	OverlayWroteToUpper   bool   `json:"overlay_wrote_to_upper"`
	AfterDropWroteToUpper bool   `json:"after_drop_wrote_to_upper"`
	MountAfterDrop        string `json:"mount_after_drop"`

	GroupFile     string `json:"group_file"`
	GroupReadable bool   `json:"group_readable"`

	Confined string   `json:"confined"`
	Problems []string `json:"problems"`
}

func inside() int {
	root := os.Getenv(rootEnv)
	out := result{UID: os.Getuid(), GID: os.Getgid()}
	out.Groups, _ = os.Getgroups()
	if profile, err := os.ReadFile("/proc/self/attr/apparmor/current"); err == nil {
		out.Confined = strings.TrimSpace(string(profile))
	}

	fail := func(format string, args ...any) {
		out.Problems = append(out.Problems, fmt.Sprintf(format, args...))
	}

	if err := nsx.Detach(); err != nil {
		fail("detaching mount propagation: %v", err)
	}
	if err := nsx.MountProc(); err != nil {
		fail("mounting /proc: %v", err)
	}

	live := filepath.Join(root, "live")
	code := filepath.Join(root, "code")
	workspace := filepath.Join(root, "workspace")
	work := filepath.Join(root, "work", "work")

	before, _ := capsx.Read(0)
	out.CapsBeforeDrop = before.String()

	options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s,userxattr",
		workspace, code, work)
	if err := unix.Mount("overlay", live, "overlay", 0, options); err != nil {
		fail("mounting the overlay: %v", err)
	} else {
		out.Mounted = true
	}

	// One read-only bind standing in the composed tree: the shape of an
	// island, and of every derived root protection. Read-only in two calls,
	// because MS_BIND|MS_RDONLY in one is silently ignored -- and with the
	// source's locked flags replicated, because a user namespace will not
	// let a remount drop them.
	island := filepath.Join(live, "CLAUDE.md")
	if err := unix.Mount(filepath.Join(workspace, "CLAUDE.md"), island, "", unix.MS_BIND, ""); err != nil {
		fail("binding the island: %v", err)
	} else if err := readOnly(island); err != nil {
		fail("making the island read-only: %v", err)
	}

	// A write to what the workspace provides has to fail loudly.
	err := os.WriteFile(island, []byte("edited\n"), 0o644)
	switch {
	case err == nil:
		out.IslandWrite = "succeeded"
	case errors.Is(err, unix.EROFS):
		out.IslandWrite = "EROFS"
	default:
		out.IslandWrite = err.Error()
	}

	// A write through the overlay has to land in the code repository.
	if err := os.WriteFile(filepath.Join(live, "born.txt"), []byte("x\n"), 0o644); err != nil {
		fail("writing through the overlay: %v", err)
	} else if _, err := os.Stat(filepath.Join(code, "born.txt")); err == nil {
		out.OverlayWroteToUpper = true
	}

	// The supplementary groups, which setgroups being denied does not take
	// away: the kernel credential keeps them and host permission checks
	// keep honouring them.
	out.GroupFile = os.Getenv("CAMP_SPIKE_GROUP_FILE")
	if out.GroupFile != "" {
		file, err := os.Open(out.GroupFile)
		if err == nil {
			buffer := make([]byte, 1)
			_, readErr := file.Read(buffer)
			out.GroupReadable = readErr == nil
			file.Close()
		}
	}

	// And now the drop.
	if err := capsx.Drop(); err != nil {
		fail("dropping capabilities: %v", err)
	}
	after, _ := capsx.Read(0)
	out.CapsAfterDrop = after.String()
	out.CapsEmptyAfter = after.Empty()

	// Nothing may be mountable any more...
	probe := filepath.Join(root, "probe")
	_ = os.MkdirAll(probe, 0o755)
	if err := unix.Mount(workspace, probe, "", unix.MS_BIND, ""); err == nil {
		out.MountAfterDrop = "succeeded"
	} else {
		out.MountAfterDrop = err.Error()
	}

	// ...while the overlay keeps working, because the kernel recorded who
	// mounted it, not who is using it.
	if err := os.WriteFile(filepath.Join(live, "after-drop.txt"), []byte("y\n"), 0o644); err != nil {
		fail("writing through the overlay after the drop: %v", err)
	} else if _, err := os.Stat(filepath.Join(code, "after-drop.txt")); err == nil {
		out.AfterDropWroteToUpper = true
	}

	encoded, _ := json.Marshal(out)
	os.Stdout.Write(encoded)
	return 0
}

func TestRouteAKeepsTheUserAndGivesTheCapabilityBack(t *testing.T) {
	identity := nsx.Own()
	if raw := os.Getenv(uidEnv); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("%s is not a number: %v", uidEnv, err)
		}
		identity.InsideUID = value
	}
	if raw := os.Getenv(gidEnv); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("%s is not a number: %v", gidEnv, err)
		}
		identity.InsideGID = value
	}

	root := scratch(t)
	attrs, err := identity.Attrs()
	if err != nil {
		t.Fatalf("building the namespace attributes: %v", err)
	}

	groupFile := groupProtectedFile()

	cmd := exec.Command(os.Args[0])
	cmd.SysProcAttr = attrs
	cmd.Env = append(os.Environ(),
		insideEnv+"=1", rootEnv+"="+root, "CAMP_SPIKE_GROUP_FILE="+groupFile)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		if isNamespaceDenied(err, stderr.String()) {
			t.Skipf("this binary may not create a user namespace, so the "+
				"identity spike cannot run from a checkout.\n"+
				"On this machine the permission is granted by an AppArmor "+
				"profile to one installed path. Install the binary and the "+
				"profile and run this again:\n"+
				"  sudo install -m 755 camp /usr/local/bin/camp\n"+
				"  sudo install -m 644 packaging/apparmor/camp /etc/apparmor.d/camp\n"+
				"  sudo apparmor_parser -r /etc/apparmor.d/camp\n"+
				"(underlying error: %v; %s)", err, strings.TrimSpace(stderr.String()))
		}
		t.Fatalf("the namespace child failed: %v\n%s", err, stderr.String())
	}

	var got result
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatalf("the child's report did not parse: %v\n%s", err, output)
	}

	// The namespace is created and then confined. That is this machine's
	// restriction working as designed, and it is not a failure of the
	// design being measured -- it is the install gate, in the one form
	// that does not announce itself as a refusal.
	if strings.HasPrefix(got.Confined, "unprivileged_userns") {
		t.Skipf("the namespace was created but AppArmor confined it to the "+
			"%q profile, which denies mounting. That is this machine's "+
			"restriction on unprivileged user namespaces; the permission is "+
			"granted by profile to one installed binary path, and a binary "+
			"built in the checkout is not that path. Install camp and its "+
			"profile and run this again:\n"+
			"  sudo install -m 755 camp /usr/local/bin/camp\n"+
			"  sudo install -m 644 packaging/apparmor/camp /etc/apparmor.d/camp\n"+
			"  sudo apparmor_parser -r /etc/apparmor.d/camp", got.Confined)
	}
	t.Logf("inside the namespace: uid=%d gid=%d groups=%v\n"+
		"  capabilities before the drop: %s\n"+
		"  capabilities after the drop:  %s",
		got.UID, got.GID, got.Groups, got.CapsBeforeDrop, got.CapsAfterDrop)

	for _, problem := range got.Problems {
		t.Errorf("inside the namespace: %s", problem)
	}

	if got.UID != identity.InsideUID || got.GID != identity.InsideGID {
		t.Errorf("inside, the process is uid %d gid %d; route A maps the "+
			"caller to itself, and the tools inside check for root",
			got.UID, got.GID)
	}
	if got.UID == 0 && identity.InsideUID != 0 {
		t.Error("the process inside is root, which is exactly what route A exists to avoid")
	}
	if !got.Mounted {
		t.Error("the overlay did not mount: the ambient capability did not " +
			"survive execve")
	}
	if got.IslandWrite != "EROFS" {
		t.Errorf("writing a workspace-provided path returned %q, wanted EROFS -- "+
			"it has to fail loudly rather than copy up anywhere", got.IslandWrite)
	}
	if !got.OverlayWroteToUpper {
		t.Error("a write through the overlay did not land in the code repository")
	}
	if !got.CapsEmptyAfter {
		t.Errorf("capabilities remain after the drop: %s", got.CapsAfterDrop)
	}
	if got.MountAfterDrop == "succeeded" {
		t.Error("mounting still worked after the drop; the capability was not given back")
	}
	if !got.AfterDropWroteToUpper {
		t.Error("the overlay stopped working after the drop; the kernel is " +
			"supposed to have recorded the mounter's credentials at mount time")
	}
	if groupFile != "" && !got.GroupReadable {
		t.Errorf("%s could not be read inside the namespace. setgroups is "+
			"denied there and the groups display as nogroup, but the kernel "+
			"credential retains them and host permission checks keep honouring "+
			"them -- this is what makes the pre-push gate run in this mode",
			groupFile)
	}
}

// groupProtectedFile returns a host file this account can only read
// through a supplementary group, or empty when there is none.
func groupProtectedFile() string {
	for _, candidate := range []string{"/var/log/syslog", "/var/run/docker.sock"} {
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		if int(stat.Uid) == os.Getuid() {
			continue
		}
		if info.Mode().Perm()&0o004 != 0 {
			continue // world-readable: it would prove nothing
		}
		if file, err := os.Open(candidate); err == nil {
			file.Close()
			return candidate
		}
	}
	return ""
}

func scratch(t *testing.T) string {
	t.Helper()
	root := testenv.Root(t)
	testenv.Write(t, filepath.Join(root, "workspace", "CLAUDE.md"), "instructions\n")
	testenv.Write(t, filepath.Join(root, "code", "src", "app.go"), "package main\n")
	testenv.MkDir(t, filepath.Join(root, "live"))
	testenv.MkDir(t, filepath.Join(root, "work", "work"))
	return root
}

func isNamespaceDenied(err error, stderr string) bool {
	if errors.Is(err, os.ErrPermission) || errors.Is(err, unix.EPERM) {
		return true
	}
	return strings.Contains(stderr, "operation not permitted") ||
		strings.Contains(err.Error(), "operation not permitted")
}

// readOnly makes one bind read-only the way camp does.
//
// The two calls are camp's rule and not this spike's: MS_BIND|MS_RDONLY
// together are silently ignored, and a remount inside a user namespace
// that drops the source mount's locked flags is refused outright.
//
// It asks mountx for those flags rather than working them out again.
// Measured, on a machine that exists for one run: with the flags written
// out by hand here and the test tree on /tmp -- nosuid,nodev, like the
// /tmp of the machine this was written on -- the remount failed with
// EPERM, the island stayed writable, and the test reported camp as letting
// a write reach the workspace. The spike is about which identity the
// process has inside the namespace; every rule it does not exist to
// measure has to come from the code that owns it.
func readOnly(target string) error {
	locked, err := mountx.LockedFlagsAt(target)
	if err != nil {
		return err
	}
	return unix.Mount("", target, "",
		uintptr(unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY)|locked, "")
}
