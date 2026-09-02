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
// refusing when it runs at a start, reporting when it runs on demand.
package verify

import (
	"bytes"
	"errors"
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
		refused.Extend(identity(mount, mount.Target, in.Table))
		refused.Extend(polarity(mount, mount.Target))
		refused.Extend(propagation(mount, mount.Target, in.Table))
	}

	refused.Extend(artefact(in))
	refused.Extend(completeness(in))
	refused.Extend(ownership(in))
	return refused
}

// The checks that run once per mount, and so can fail for several mounts
// in one pass. A composition that went wrong went wrong the same way at
// every mount of its kind -- a whole sequence propagating, a whole
// sequence writable -- and one paragraph with the list of paths is the
// report that can be acted on.
//
// Nothing in a Detail names a path: that is what lets the mounts gather
// onto one refusal.
var (
	unreachableSources = refusal.Group{
		Rule: "verify-source-unreachable",
		One:  "a mount source cannot be looked at after mounting:",
		Many: "%d mount sources cannot be looked at after mounting:",
		Detail: "The source was there when the plan was checked. Something has " +
			"changed it since, and camp will not call a composition verified " +
			"against a source it cannot see.",
	}
	unreachableTargets = refusal.Group{
		Rule: "verify-target-unreachable",
		One:  "a mount point cannot be looked at after mounting:",
		Many: "%d mount points cannot be looked at after mounting:",
		Detail: "The path is the authority here, not the kernel's table: a covered " +
			"mount stays listed in the table and is reachable by nothing.",
	}
	wrongIdentity = refusal.Group{
		Rule: "verify-identity",
		One:  "a mount point does not show what was mounted on it:",
		Many: "%d mount points do not show what was mounted on them:",
		Detail: "Either the mount did not take, or a later mount is covering it " +
			"-- a covered mount stays listed in the kernel's table and is " +
			"reachable by nothing, so the table would not have shown this.",
	}
	unreachablePolarity = refusal.Group{
		Rule: "verify-polarity-unreachable",
		One:  "a mount cannot be asked whether it is writable:",
		Many: "%d mounts cannot be asked whether they are writable:",
		Detail: "statvfs is what answers that question the way a process writing " +
			"there would experience the answer, and it did not answer.",
	}
	writable = refusal.Group{
		Rule: "verify-writable",
		One:  "a mount was made read-only and is writable:",
		Many: "%d mounts were made read-only and are writable:",
		Detail: "This is the failure the whole arrangement exists to prevent: a " +
			"write there would copy the file up into the code repository instead " +
			"of failing. It happens when a bind and its read-only flag are asked " +
			"for in one call, which the kernel accepts and silently ignores.",
	}
	readOnlyByMistake = refusal.Group{
		Rule: "verify-read-only",
		One:  "a mount was made writable and is read-only:",
		Many: "%d mounts were made writable and are read-only:",
		Detail: "Writes meant to land there will fail. A bind is exactly as " +
			"writable as the mount it was cut from, so one of two things is true " +
			"of the source: its filesystem is mounted read-only, or something " +
			"read-only already stood on its path when this bind was made. Look at " +
			"what the source is mounted on, from outside a session.",
	}
	propagating = refusal.Group{
		Rule: "verify-propagation",
		One:  "a mount propagates:",
		Many: "%d mounts propagate:",
		Detail: "Every camp mount is made private as it is created: a propagating " +
			"mount inside the composed tree travels back out onto the backing " +
			"store's own path, which is how eight planned mounts once became " +
			"twelve, four of them on the workspace's path.",
	}
	overlayOptionsWrong = refusal.Group{
		Rule: "verify-overlay-option",
		One: "the composed tree was mounted with an option the plan did not ask " +
			"for:",
		Many: "the composed tree was mounted with %d options the plan did not ask " +
			"for:",
		Detail: "The options are compared one by one and never as one string: the " +
			"kernel echoes what was passed plus its own defaults, so string " +
			"equality would fail on a correct mount every time.",
	}
)

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
		refused.Group(unreachableSources, "%s: %v", mount.Source, err)
		return refused
	}
	destination, err := stat(target)
	if err != nil {
		refused.Group(unreachableTargets, "%s: %v", target, err)
		return refused
	}
	if source != destination {
		refused.Group(wrongIdentity, "%s should show %s: the path resolves to "+
			"%s, and the source is %s", target, mount.Source, destination, source)
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

// overlayOptions compares the mounted overlay with the description the
// mount was performed from.
//
// The expectation is not composed here. mountx derives what the kernel
// is told about an overlay once, performs the mount from that
// derivation, and compares against it -- so this pass cannot hold a
// mount to operands the mount was never asked for, which is exactly what
// a second expectation written out of the same plan fields was free to
// do.
//
// The comparison is per operand and never one string: the kernel echoes
// what was passed plus its own defaults -- redirect_dir, uuid, and
// userxattr inside a user namespace whether or not it was asked for --
// so string equality would fail on a correct mount every time.
func overlayOptions(mount plan.Mount, target string, table []mountinfo.Entry) refusal.List {
	var refused refusal.List
	entry, found := mountinfo.Top(table, target)
	if !found {
		refused.Add("verify-overlay-untabled",
			"the composed tree at %s answers as an overlay but the kernel's "+
				"mount table has no entry for it.", target)
		return refused
	}

	for _, wrong := range mountx.DescribeOverlay(mount).Mismatches(entry) {
		if wrong.Flag {
			refused.Add("verify-overlay-xattr",
				"the composed tree was mounted without %s. The extended-attribute "+
					"namespace is not a preference: a mount made inside a user "+
					"namespace cannot write trusted.* at all, and one made as root "+
					"must not use user.*.", wrong.Key)
			continue
		}
		refused.Group(overlayOptionsWrong, "%s is %q and the plan said %q",
			wrong.Key, wrong.Got, wrong.Want)
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
		refused.Group(unreachablePolarity, "%s: %v", target, err)
		return refused
	}

	readOnly := fs.Flags&unix.ST_RDONLY != 0
	wantReadOnly := mount.Kind == plan.BindRO
	switch {
	case wantReadOnly && !readOnly:
		refused.Group(writable, "%s", target)
	case !wantReadOnly && readOnly:
		refused.Group(readOnlyByMistake, "%s", target)
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
		refused.Group(propagating, "%s (%s)", target, strings.Join(entry.Optional, " "))
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
	path := filepath.Join(in.Plan.Live, ".git", "info", "exclude")
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
//
// Three places are enumerated, because the plan has mounts in three
// places: under the composed tree, on the workspace's own path and on the
// code repository's own path -- the two self-binds that live outside the
// tree.
func completeness(in Input) refusal.List {
	var refused refusal.List

	present := map[string]bool{}
	for _, entry := range mountinfo.Under(in.Table, in.Plan.Live) {
		present[entry.Point] = true
	}
	for _, entry := range mountinfo.At(in.Table, in.Plan.Config.LowerPath()) {
		present[entry.Point] = true
	}
	for _, entry := range mountinfo.At(in.Table, in.Plan.Config.UpperPath()) {
		present[entry.Point] = true
	}

	planned := map[string]bool{}
	for _, mount := range in.Plan.Mounts {
		planned[mount.Target] = true
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
				"have: %s.\ncamp did not make them: a session's mounts exist only "+
				"inside its own namespace and go with it, so these were mounted by "+
				"something else, into this tree or onto a path under it before the "+
				"session started. camp will not declare a composition up that it "+
				"cannot account for.", strings.Join(sorted(extra), ", "))
	}
	return refused
}

// ownership checks that camp's persistent storage belongs to the
// invoking user.
//
// It is the one place the design guarantees writable. Everything under it
// is created by a process running as the user, so this is a check that
// what the design says is true rather than a repair -- and a check that
// would have been the whole difference had a process with privilege ever
// created anything there.
//
// Every path camp made there is checked, not only the root: the store of
// each islands mount, and the attachment point of each island. One of
// them owned by anybody else is a mount camp guarantees writable that
// nobody can write. What is deliberately not walked is everything else
// under storage -- worktrees, machine-local files, a person's own work --
// because that is the user's, it can be enormous, and its ownership is
// not camp's claim to make.
func ownership(in Input) refusal.List {
	var refused refusal.List
	storage := in.Plan.Storage
	if storage == "" {
		return refused
	}
	for _, mount := range in.Plan.Mounts {
		if mount.Role != plan.Store && mount.Role != plan.Island {
			continue
		}
		if mount.Source == "" || !strings.HasPrefix(mount.Source, storage) {
			continue
		}
		refused.Extend(owns(mount.Source, in))
	}
	refused.Extend(owns(storage, in))
	return refused
}

// owns is the one question, asked of one path.
func owns(path string, in Input) refusal.List {
	var refused refusal.List
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			// It need not exist: a composition may use no storage at all, and
			// then there is nothing to own.
			return refused
		}
		// Anything else is the check not running, which is not the same as
		// the check passing. This is the one path the design guarantees
		// writable.
		refused.Add("verify-storage-unreadable",
			"camp's storage path %s could not be looked at: %v.\n"+
				"That directory holds worktrees and machine-local files you have "+
				"to be able to write, and camp checks at every start that it is "+
				"still yours. A check that cannot run is not a check that passed.",
			path, err)
		return refused
	}
	if int(st.Uid) != in.UID || int(st.Gid) != in.GID {
		refused.Add("verify-storage-owner",
			"camp's storage path %s is owned by uid %d gid %d and should belong "+
				"to uid %d gid %d.\nStorage holds worktrees and machine-local "+
				"files you have to be able to write, and camp creates everything "+
				"there as you.",
			path, st.Uid, st.Gid, in.UID, in.GID)
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
