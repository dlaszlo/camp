// Package verify checks what the kernel actually did.
//
// Two measured facts decide its whole shape. A covered mount stays listed
// in /proc/self/mountinfo while being unreachable by any path, so
// presence in the table proves nothing about what a process sees. And
// path-based syscalls see exactly what a process would. So **the path is
// the authority and mountinfo is the cross-check** -- never the other way
// round.
//
// The second habit: never trust the call, inspect the result.
// MS_BIND|MS_RDONLY in one mount(2) silently ignores the read-only flag,
// so a bind that reports success can be writable. statvfs answers that
// question the way the process asking it would experience the answer.
//
// status is this same pass run read-only: one code path, two exits --
// refusing when it runs at up, reporting when it runs on demand.
package verify

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/mountx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
)

// OverlayMagic identifies an overlay filesystem to statfs, which is the
// programmatic equivalent of reading "overlay" out of mountinfo's fstype
// field.
const OverlayMagic = 0x794c7630

// Input is everything one pass needs.
type Input struct {
	Plan plan.Plan
	// Prefix is where the tree is right now. It is the live path in the
	// namespace mode and in the privileged mode's second pass, and the
	// staging root in the privileged mode's first.
	Prefix string
	// Table is the mount table as it stands.
	Table []mountinfo.Entry
	// Exclude is the payload the generated exclude must equal, byte for
	// byte. Empty when the configuration has no generation step, and then
	// nothing is checked for it -- a composition is not held to a promise
	// it never made.
	Exclude []byte
	// UID and GID are who storage has to belong to.
	UID int
	GID int
}

// Run performs the whole pass and returns everything that is wrong.
//
// Everything, not the first thing: a composition with three problems
// should be reported once, not discovered three times.
func Run(in Input) refusal.List {
	var refused refusal.List

	for _, mount := range in.Plan.Mounts {
		target := at(in, mount)
		refused.Extend(identity(mount, target, in.Table))
		refused.Extend(polarity(mount, target))
		refused.Extend(propagation(mount, target, in.Table))
	}

	refused.Extend(artefact(in))
	refused.Extend(completeness(in))
	refused.Extend(ownership(in))
	return refused
}

// at returns where a mount stands during this pass. Everything inside the
// tree moves with the prefix; the workspace's self-bind does not, because
// it is not in the tree.
func at(in Input, mount plan.Mount) string {
	if !mount.InLive {
		return mount.Target
	}
	if mount.Rel.Empty() {
		return in.Prefix
	}
	return mount.Rel.Join(in.Prefix)
}

// identity asks whether the mount is the one that was planned, and
// whether it is reachable at all.
//
// For a bind, source and target must be the same object -- the same
// device and inode. That single check also catches every ordering and
// shadowing mistake, because a mount that something else covers fails it:
// the path resolves to the coverer, not to the source.
func identity(mount plan.Mount, target string, table []mountinfo.Entry) refusal.List {
	var refused refusal.List

	if mount.Kind == plan.Overlay {
		var fs unix.Statfs_t
		if err := unix.Statfs(target, &fs); err != nil {
			refused.Add("verify-overlay-unreachable",
				"the composed tree at %s cannot be looked at: %v.", target, err)
			return refused
		}
		if int64(fs.Type) != OverlayMagic {
			refused.Add("verify-overlay-missing",
				"%s is not an overlay filesystem. The composed tree was mounted "+
					"and something else is standing there now.", target)
			return refused
		}
		refused.Extend(overlayOptions(mount, target, table))
		return refused
	}

	source, err := stat(mount.Source)
	if err != nil {
		refused.Add("verify-source-unreachable",
			"the mount source %s cannot be looked at after mounting: %v.", mount.Source, err)
		return refused
	}
	destination, err := stat(target)
	if err != nil {
		refused.Add("verify-target-unreachable",
			"the mount point %s cannot be looked at after mounting: %v.", target, err)
		return refused
	}
	if source != destination {
		refused.Add("verify-identity",
			"%s does not show %s: the path resolves to %s, and the source is "+
				"%s.\nEither the mount did not take, or a later mount is covering "+
				"it -- a covered mount stays listed in the kernel's table and is "+
				"reachable by nothing, so the table would not have shown this.",
			target, mount.Source, destination, source)
	}
	return refused
}

type ident struct {
	device uint64
	inode  uint64
}

func (i ident) String() string { return fmt.Sprintf("%d:%d", i.device, i.inode) }

func stat(path string) (ident, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return ident{}, err
	}
	return ident{device: uint64(st.Dev), inode: st.Ino}, nil
}

// overlayOptions compares the overlay's options per option.
//
// Never as one string: the kernel echoes what was passed plus its own
// defaults -- redirect_dir, uuid, and userxattr inside a user namespace
// whether or not it was asked for -- so string equality would fail on a
// correct mount every time.
func overlayOptions(mount plan.Mount, target string, table []mountinfo.Entry) refusal.List {
	var refused refusal.List
	entry, found := mountinfo.Top(table, target)
	if !found {
		refused.Add("verify-overlay-untabled",
			"the composed tree at %s answers as an overlay but the kernel's "+
				"mount table has no entry for it.", target)
		return refused
	}

	want := map[string]string{
		"lowerdir": strings.Join(mount.Lower, ":"),
		"upperdir": mount.Upper,
		"workdir":  mount.Work,
	}
	for key, expected := range want {
		got := mountinfo.UnescapeOption(entry.Super[key])
		if got != expected {
			refused.Add("verify-overlay-option",
				"the composed tree's %s is %q and the plan said %q.", key, got, expected)
		}
	}
	if mount.Xattr != "" {
		if _, present := entry.Super[mount.Xattr]; !present {
			refused.Add("verify-overlay-xattr",
				"the composed tree was mounted without %s. The extended-attribute "+
					"namespace is not a preference: a mount made inside a user "+
					"namespace cannot write trusted.* at all, and one made as root "+
					"must not use user.*.", mount.Xattr)
		}
	}
	return refused
}

// polarity asks whether each mount is writable exactly where it should
// be, as a process would experience it.
//
// This is what catches a one-step read-only bind: the mount(2) call
// reported success and silently ignored the read-only flag, and only the
// result says so.
func polarity(mount plan.Mount, target string) refusal.List {
	var refused refusal.List
	var fs unix.Statfs_t
	if err := unix.Statfs(target, &fs); err != nil {
		refused.Add("verify-polarity-unreachable",
			"%s cannot be looked at: %v.", target, err)
		return refused
	}

	readOnly := fs.Flags&unix.ST_RDONLY != 0
	wantReadOnly := mount.Kind == plan.BindRO
	switch {
	case wantReadOnly && !readOnly:
		refused.Add("verify-writable",
			"%s was mounted read-only and is writable.\nThis is the failure the "+
				"whole arrangement exists to prevent: a write there would copy the "+
				"file up into the code repository instead of failing. It happens "+
				"when a bind and its read-only flag are asked for in one call, "+
				"which the kernel accepts and silently ignores.", target)
	case !wantReadOnly && readOnly:
		refused.Add("verify-read-only",
			"%s was mounted writable and is read-only. Writes meant to land "+
				"there will fail.", target)
	}
	return refused
}

// propagation asks whether the mount is private.
//
// Mounts propagate by default on a systemd machine, and a propagating
// mount inside the composed tree travels back onto the backing store's
// own path. Empty optional fields in the table is exactly what private
// means.
func propagation(mount plan.Mount, target string, table []mountinfo.Entry) refusal.List {
	var refused refusal.List
	entry, found := mountinfo.Top(table, target)
	if !found {
		return refused
	}
	if !entry.Private() {
		refused.Add("verify-propagation",
			"%s propagates (%s). Every camp mount is made private as it is "+
				"created: a propagating mount inside the composed tree travels "+
				"back out onto the backing store's own path, which is how eight "+
				"planned mounts once became twelve, four of them on the "+
				"workspace's path.", target, strings.Join(entry.Optional, " "))
	}
	return refused
}

// artefact compares the generated exclude, byte for byte, with the
// payload that was validated.
//
// Byte for byte and not by its marker line: a payload whose repository
// half had been dropped would still carry the marker, and the check would
// pass while the composed tree's git quietly stopped honouring the
// repository's own exclude lines.
func artefact(in Input) refusal.List {
	var refused refusal.List
	if len(in.Exclude) == 0 {
		return refused
	}
	path := filepath.Join(in.Prefix, ".git", "info", "exclude")
	got, err := os.ReadFile(path)
	if err != nil {
		refused.Add("verify-exclude-unreadable",
			"the generated exclude at %s could not be read: %v.", path, err)
		return refused
	}
	if !bytes.Equal(got, in.Exclude) {
		refused.Add("verify-exclude",
			"%s does not contain the payload camp generated and validated: "+
				"%d bytes are there and %d were expected.\nWhat git reads through "+
				"the composed tree has to be exactly what camp checked, or the "+
				"check was about a different file.", path, len(got), len(in.Exclude))
	}
	return refused
}

// completeness compares the set of mounts that exist with the set that
// was planned.
//
// Fewer means a mount failed; more means residue, or another composition,
// or something else interfering. Either way the plan and the machine
// disagree, and the teardown list would be wrong.
func completeness(in Input) refusal.List {
	var refused refusal.List

	present := map[string]bool{}
	for _, entry := range mountinfo.Under(in.Table, in.Prefix) {
		present[entry.Point] = true
	}
	for _, entry := range mountinfo.At(in.Table, in.Plan.Config.LowerPath()) {
		present[entry.Point] = true
	}

	planned := map[string]bool{}
	for _, mount := range in.Plan.Mounts {
		planned[at(in, mount)] = true
	}

	var missing, extra []string
	for point := range planned {
		if !present[point] {
			missing = append(missing, point)
		}
	}
	for point := range present {
		if !planned[point] {
			extra = append(extra, point)
		}
	}

	if len(missing) > 0 {
		refused.Add("verify-missing-mounts",
			"the plan has mounts the kernel does not: %s.", strings.Join(sorted(missing), ", "))
	}
	if len(extra) > 0 {
		refused.Add("verify-extra-mounts",
			"there are mounts under the composed tree that the plan does not "+
				"have: %s.\nThat is residue from a run that did not come down, or "+
				"another composition, or something outside camp mounting into this "+
				"tree. camp will not declare a composition up that it cannot "+
				"account for.", strings.Join(sorted(extra), ", "))
	}
	return refused
}

// ownership checks that camp's persistent storage belongs to the
// invoking user.
//
// It is the one place the design guarantees writable, and in the
// privileged mode the temptation for it to end up root-owned is real:
// the helper runs as root. It creates nothing there, and this is what
// proves it.
func ownership(in Input) refusal.List {
	var refused refusal.List
	if in.Plan.Storage == "" {
		return refused
	}
	var st unix.Stat_t
	if err := unix.Stat(in.Plan.Storage, &st); err != nil {
		return refused // it need not exist; a composition may use no storage
	}
	if int(st.Uid) != in.UID || int(st.Gid) != in.GID {
		refused.Add("verify-storage-owner",
			"camp's storage %s is owned by uid %d gid %d and should belong to "+
				"uid %d gid %d.\nThat directory holds worktrees and machine-local "+
				"files you have to be able to write. The privileged helper creates "+
				"nothing there for exactly this reason.",
			in.Plan.Storage, st.Uid, st.Gid, in.UID, in.GID)
	}
	return refused
}

func sorted(values []string) []string {
	out := append([]string(nil), values...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// LockedFlagsFeasible reports whether a read-only remount can be made on
// the filesystem a path sits on, and what it would have to replicate.
//
// Reported rather than refused: a nosuid filesystem is supported now,
// because the flags are replicated. What doctor says about it is
// information -- a noexec environment cannot run scripts out of the tree
// -- and never a reason to stop.
func LockedFlagsFeasible(table []mountinfo.Entry, path string) (string, bool) {
	entry, found := mountinfo.Containing(table, path)
	if !found {
		return "", false
	}
	return mountx.DescribeFlags(mountx.LockedFlags(entry)), true
}
