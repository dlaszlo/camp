// Package privileged is the fallback mode: one mount table, visible to
// every process on the machine.
//
// Its shape is an unprivileged front end and one narrow privileged
// helper, and the reason is not tidiness. A process that is root from its
// first instruction has no "before sudo" in which the generation step
// could run as the user -- and configured code must never run with
// privilege, because whoever can edit the configuration would then be
// able to gain root through it.
//
// So 'camp up' runs as the invoking user from start to finish: it locks,
// validates, gates, generates, validates what was generated, writes the
// record -- and then invokes the helper as 'sudo camp helper-mount', an
// internal subcommand that does exactly one thing: execute the validated
// concrete plan handed to it on **stdin**. Never argv: /proc exposes argv
// machine-wide. The helper reads no configuration, runs no generator and
// consults no state.
//
// The helper trusts nothing it was handed. The user owns every parent
// directory of every operand, so a component can become a symlink between
// the front end's check and the helper's mount(2) -- and the check would
// then have been about a different object than the mount. The helper
// re-resolves every operand itself, descriptor-relative, following no
// symlink and never leaving the recorded base, verifies each endpoint's
// device and inode and its kind against the plan, and mounts by
// descriptor. Any mismatch fails closed.
//
// The base is the first of those descriptors and the one the rest hang
// off. It is opened once, following nothing, checked for the invoking
// user's ownership on that descriptor, and held for the whole
// invocation; nothing after that names it again. A base checked by name
// and used by name is two directories the moment its owner renames it,
// and its owner is exactly the person the helper is acting for -- which
// made every strict walk beneath it a strict walk beneath whatever the
// name reached at that instant.
//
// sudo is exercised exactly once per command. That is why the helper also
// runs the first verification pass and performs the move: the sequence
// mount, verify, move has to happen without giving the privilege back and
// asking for it again in the middle.
package privileged

import (
	"github.com/dlaszlo/camp/internal/plan"
)

// JobVersion is the wire format between the front end and the helper.
// Both halves are the same binary, so a mismatch means someone is running
// two builds; the helper refuses rather than reading fields that moved.
const JobVersion = 1

// Action is what the helper was asked to do.
type Action string

const (
	// ActionMount executes a plan, verifies it in staging and moves it
	// onto the live directory.
	ActionMount Action = "mount"
	// ActionUnmount removes a recorded list of targets, in the order
	// given.
	ActionUnmount Action = "unmount"
)

// Job is the whole instruction, and the only thing the helper reads.
type Job struct {
	Version int    `json:"version"`
	Action  Action `json:"action"`

	// Base is the environment root, already resolved. The helper opens it
	// once, following nothing -- it is already resolved, so a symlink at
	// its final component was put there afterwards -- checks the invoking
	// user owns the descriptor it got, and holds that descriptor for the
	// whole invocation. Every operand is addressed as components beneath
	// it, opened one component at a time from that descriptor, following
	// nothing. Nothing the helper does resolves this string a second time.
	Base string `json:"base"`

	// UID and GID are the invoking user's. Everything the helper touches
	// that has to stay writable ends up theirs.
	//
	// The helper overwrites both from SUDO_UID and SUDO_GID and never
	// honours what arrives here: a uid in a job is a request for root to
	// hand something to somebody, and the job comes from an unprivileged
	// process. They stay in the shape because the front end fills the
	// record from the same two numbers and a job that disagreed with its
	// record would be worth noticing.
	UID int `json:"uid"`
	GID int `json:"gid"`

	// Mounts is the concrete plan, in order. Mount only.
	Mounts []JobMount `json:"mounts,omitempty"`

	// StagingParts is where the tree is built, beneath Base. Mount only.
	StagingParts []string `json:"staging_parts,omitempty"`
	// LiveParts is where it is moved to. Mount only.
	LiveParts []string `json:"live_parts,omitempty"`

	// LowerPath is the workspace's own path, and Storage camp's persistent
	// store. Mount only.
	//
	// The helper reads no configuration, so everything its verification
	// needs has to travel with the job. When these did not, the pass ran
	// against a zero value and reported the frame's first mount missing on
	// every honest composition.
	LowerPath string `json:"lower_path,omitempty"`
	Storage   string `json:"storage,omitempty"`
	// Exclude is the payload the generated exclude must equal, byte for
	// byte, in the staging tree. Mount only, and empty when the
	// composition generates none.
	Exclude []byte `json:"exclude,omitempty"`

	// Targets are what to unmount, in teardown order. Unmount only.
	Targets []JobTarget `json:"targets,omitempty"`
	// WorkParts is camp's work directory, beneath Base: the kernel leaves
	// a root-owned directory inside it that only the helper can remove.
	WorkParts []string `json:"work_parts,omitempty"`
}

// JobTarget is one mount to remove, with the identity camp's own mount
// answered as when it was made.
//
// The identity travels because a path is not proof of anything: camp's
// mount may be gone and somebody else's may stand at the same name, and
// unmounting that one is a stranger's mount removed by root on camp's
// say-so. The helper compares before it unmounts.
type JobTarget struct {
	Path string `json:"path"`
	// Device and Inode are what the front end recorded for this mount.
	// Both zero means the record has no identity for it -- a record
	// written before its mount was made, which a crash can leave behind --
	// and then the helper unmounts what it was given, because refusing
	// would leave somebody walled in behind mounts camp made.
	Device uint64 `json:"device,omitempty"`
	Inode  uint64 `json:"inode,omitempty"`
}

// JobMount is one operation, with everything needed to re-check it.
type JobMount struct {
	Kind string `json:"kind"`
	Role string `json:"role"`

	// Source and Target are absolute, for messages and for the overlay's
	// option string.
	Source string `json:"source,omitempty"`
	Target string `json:"target"`

	// SourceParts and TargetParts are the same two paths as components
	// beneath the job's base. These are what the helper resolves.
	SourceParts []string `json:"source_parts,omitempty"`
	TargetParts []string `json:"target_parts"`

	// SourceIdent and TargetIdent are the device and inode the front end
	// saw. The helper compares what it opened against them, and refuses
	// when they differ -- that is the rename-and-symlink race, closed.
	SourceIdent string `json:"source_ident,omitempty"`
	TargetIdent string `json:"target_ident,omitempty"`
	// TargetAbsent says the mount point did not exist yet when the job was
	// built, which is ordinary: most of them are supplied by an earlier
	// mount in the same sequence, inside the staging tree.
	//
	// It exists so that a missing identity means one thing rather than
	// two, and the helper requires it: a mount point with no identity is
	// refused unless it is inside the staging tree *and* this says why.
	// An identity that was simply empty used to be accepted, so a target
	// the front end could not look at -- for any reason -- was mounted
	// onto without a single check, and the difference between "not there
	// yet" and "camp could not tell" was invisible on the wire.
	TargetAbsent bool `json:"target_absent,omitempty"`

	// The overlay's operands, as paths for the messages and the record.
	Lower []string `json:"lower,omitempty"`
	Upper string   `json:"upper,omitempty"`
	Work  string   `json:"work,omitempty"`
	Xattr string   `json:"xattr,omitempty"`

	// The same three as components beneath the job's base, with the
	// identity the front end saw for each.
	//
	// These are what the helper resolves and compares. Without them the
	// overlay was the one operation whose operands crossed as bare paths:
	// the bind endpoints were opened and checked, and then the composed
	// tree -- the mount that decides what the whole composition shows and
	// where writes land -- was created from three strings the kernel
	// resolved again, at mount time, following whatever was there then.
	LowerParts  [][]string `json:"lower_parts,omitempty"`
	LowerIdents []string   `json:"lower_idents,omitempty"`
	UpperParts  []string   `json:"upper_parts,omitempty"`
	UpperIdent  string     `json:"upper_ident,omitempty"`
	WorkParts   []string   `json:"work_parts,omitempty"`
	WorkIdent   string     `json:"work_ident,omitempty"`

	// SourceType is what the front end saw the source to be, in pathx's
	// vocabulary: "directory" or "file", and nothing else -- camp binds
	// neither a socket nor a device.
	//
	// Required on every non-overlay operation that names a source, and
	// compared against the descriptor the helper is about to bind: a
	// directory binds onto a directory and a file onto a file, and the
	// kernel refuses to mix them. So the kind is part of what makes an
	// operand the operand camp planned, and a source that changed kind
	// after the front end looked is refused with nothing mounted. It
	// travels beside SourceIdent and comes from the same single look, so
	// the job can never carry the identity of one object and the kind of
	// another.
	SourceType string `json:"source_type,omitempty"`
}

// Reply is what the helper reports back on stdout.
type Reply struct {
	Version int `json:"version"`
	// Results is one entry per operation attempted, in order.
	Results []Result `json:"results"`
	// Error is why the whole job stopped, empty when it did not.
	Error string `json:"error,omitempty"`
	// Rule is the short, stable identifier of what refused, so that the
	// tests hold to something that does not move when the prose does.
	Rule string `json:"rule,omitempty"`
	// RolledBack is true when a failure was unwound completely, so the
	// front end knows the machine is clean.
	RolledBack bool `json:"rolled_back,omitempty"`
	// Stranded is what a failed rollback could not remove.
	Stranded []string `json:"stranded,omitempty"`
	// Moved is true once the verified tree stands at the live path.
	Moved bool `json:"moved,omitempty"`
}

// Result is what happened to one operation.
type Result struct {
	Target string `json:"target"`
	Device uint64 `json:"device,omitempty"`
	Inode  uint64 `json:"inode,omitempty"`
	// Outcome is "mounted", "unmounted", "absent", "busy", or "mismatch"
	// for a target the helper refused to touch because what stands there
	// is not what camp put there.
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

// AsMount rebuilds the plan mount a helper operation describes, so that
// mountx can execute it without knowing about the wire format.
func (m JobMount) AsMount(target string) plan.Mount {
	return plan.Mount{
		Kind:   plan.Kind(m.Kind),
		Role:   plan.Role(m.Role),
		Source: m.Source,
		Target: target,
		Lower:  m.Lower,
		Upper:  m.Upper,
		Work:   m.Work,
		Xattr:  m.Xattr,
	}
}
