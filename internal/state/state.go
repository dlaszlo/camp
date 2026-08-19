// Package state records what the privileged mode mounted, so that a run
// which died halfway can still be undone.
//
// The record is what crash recovery stands on, and everything about it
// follows from that.
//
// It carries the **complete concrete plan, in order**, not a reference to
// the configuration. down, status and explain read the record and never
// the configuration, because the configuration may have been edited while
// the composition was up -- and then the file that says what to unmount
// would describe a composition nobody built.
//
// It is written **before** the helper mounts anything, in phase
// "mounting", and moves to "up" only after the verification at the live
// path passes. A failure with a clean rollback removes it; a failed
// rollback leaves "partial" with the plan intact. So there is no moment
// at which something is mounted and nothing knows what.
//
// That claim is about two places and not one. The privileged mode builds
// the composition inside a staging directory and moves the whole tree
// onto the live path in a single step, so until the move every mount of
// it stands in staging, under a staging point the helper bound onto
// itself first of all -- which is most of the helper's life. The record
// therefore names **both places for every mount**, and both self-binds,
// and treats them as two possible locations of one mount. It cannot know
// which: the move happens inside the helper, and a kill lands on either
// side of it. Naming a place with nothing at it costs one "absent"
// answer; a mount at a place nothing names is a mount camp cannot
// recover, and that is the failure this shape exists to prevent.
//
// A record is never discarded while anything it answers for is still
// mounted -- at one of its places, or anywhere in the work, staging or
// live tree. Release makes that decision, and every command that forgets
// a record makes it there.
//
// It is written and read **only by the unprivileged front end**. sudo
// wraps the helper alone, so XDG_STATE_HOME and the home directory always
// resolve in the invoking user's environment, and the
// root-home-versus-user-home ambiguity cannot arise.
//
// The namespace mode has no record at all and needs none: the namespace
// is the state, and it vanishes with its last process.
package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/mountx"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
)

// Version is the schema this build writes. A reader refuses a version it
// does not know rather than guessing at a field that moved.
const Version = 1

// Phase is how far a composition got, and what has to happen next.
type Phase string

const (
	// Mounting means the plan is written and the helper is working, or
	// died while working. Something may be mounted.
	Mounting Phase = "mounting"
	// Up means every mount is made and the verification at the live path
	// passed.
	Up Phase = "up"
	// Partial means a teardown or a rollback could not finish. Mounts
	// remain, and the plan that names them is still here.
	Partial Phase = "partial"
	// Down means everything the plan named is gone.
	Down Phase = "down"
)

// Active reports whether a phase means something may still be mounted.
func (p Phase) Active() bool { return p == Mounting || p == Up || p == Partial }

// Mount is one operation as it was planned, and as it turned out.
type Mount struct {
	Kind   string `json:"kind"`
	Role   string `json:"role"`
	Source string `json:"source"`
	Target string `json:"target"`
	// Staging is where this mount stands until the tree is moved onto the
	// live path, which is where it stands for the whole of the helper's
	// work. Target and this are the only two places it can ever be, and
	// which of the two it is in is not something the record can know: the
	// move happens inside the helper, and a kill lands on either side of
	// it.
	//
	// Empty means there is one place and this is not two of them. That is
	// the mount outside the tree -- the workspace's self-bind, made at its
	// final path -- and it is also every mount of a record written before
	// this field existed. Such a record still decodes, and a teardown
	// derived from it names what it always named, the live targets and the
	// live self-bind, and nothing more. Not a degradation to be silent
	// about: it is what an older record can support, and it is exactly
	// what it supported when it was written.
	Staging string `json:"staging,omitempty"`
	// Options is the overlay's option string, empty for a bind.
	Options string `json:"options,omitempty"`
	// FSType is what the mount should answer as.
	FSType string `json:"fstype,omitempty"`
	// Device and Inode are the target's identity once it is mounted. Zero
	// until then, which is itself information: this mount had not happened
	// when the record was last written.
	Device uint64 `json:"device,omitempty"`
	Inode  uint64 `json:"inode,omitempty"`
}

// Record is one composition.
type Record struct {
	Version int `json:"version"`

	UID int `json:"uid"`
	GID int `json:"gid"`

	Config    string `json:"config"`
	Env       string `json:"env"`
	Live      string `json:"live"`
	Upper     string `json:"upper"`
	Workspace string `json:"workspace"`
	Hash      string `json:"hash"`

	// ConfigDigest and InventoryDigest are SHA-256 over the bytes of each
	// file as they were at up. They are what lets down report drift
	// without trusting the current file.
	ConfigDigest    string `json:"config_digest"`
	InventoryDigest string `json:"inventory_digest"`

	// Mounts is the complete concrete plan, in the order it was made. Its
	// reverse is the teardown order.
	Mounts []Mount `json:"mounts"`

	// Created is what camp made for this composition and may clear again:
	// the work area, and the overlay work directory inside it that the
	// kernel fills with a root-owned leftover.
	//
	// Storage is deliberately not here, and neither are the attachment
	// points camp scaffolds inside it. That directory holds half-done work
	// and camp never removes it (invariant 3), so a list headed "what camp
	// may clear" must not name it; what camp put there is recorded in the
	// islands manifest, which lives beside it and survives this record.
	Created []string `json:"created"`

	// Staging is where the tree is built before the move, in the
	// privileged mode. Every mount's staging location is inside it, and it
	// is itself a mount: the helper binds it onto itself before it builds
	// anything, because MS_MOVE refuses a mount whose parent is shared.
	Staging string `json:"staging,omitempty"`

	// Detached are the mount points the privileged helper bound onto
	// themselves so that what was moved onto them could not propagate, in
	// the order they are made: the staging point first, the live point
	// second. They sit underneath the composition and come off after it,
	// so a teardown removes them last and in the reverse of this order.
	Detached []string `json:"detached,omitempty"`

	// Stranded are locations a rollback or a teardown could not remove.
	//
	// Written down rather than only said. The helper names them in its
	// reply, the front end printed them in a sentence and dropped them,
	// and the next 'camp down' has nothing but this record to address them
	// by -- so a mount that survived a failed rollback was a mount camp
	// could describe once and never remove.
	Stranded []string `json:"stranded,omitempty"`

	Phase Phase `json:"phase"`

	Tool      string `json:"tool_version"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Dir is where records live: the invoking user's state directory, always
// resolved in the invoking user's environment.
func Dir() string {
	base, name := where()
	return filepath.Join(base, name)
}

// where splits that into the directory camp writes in and camp's own name
// inside it.
//
// Split because the two halves are trusted differently. The user's state
// directory is the user's -- camp neither made it nor vouches for it --
// and camp's own directory below it is resolved from there following no
// symlink, like every other place camp writes.
func where() (string, string) {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return base, "camp"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return os.TempDir(), "camp"
	}
	return filepath.Join(home, ".local", "state"), "camp"
}

// held is the state base, resolved once and kept open, and the name it
// was opened from.
//
// The base comes from the ambient environment rather than from any
// caller, so there is nowhere else to open it and nobody to hand it to:
// the package resolves it the first time a record is written and keeps
// the capability for the rest of the process. Reopening it per write
// would resolve XDG_STATE_HOME again every time, which is the hole this
// closes -- a link swapped in at that name between two records would put
// the second one somewhere else.
var (
	heldMutex sync.Mutex
	held      pathx.Root
	heldName  string
)

// root is the state base as a capability: resolved once, held open.
//
// The base has to exist already, which is not a new condition -- writing
// a record opened the base before creating anything below it -- but the
// failure is named here, because "camp could not open your state
// directory" is a sentence about a specific directory and the reader has
// to be told which.
func root() (pathx.Root, error) {
	base, _ := where()
	heldMutex.Lock()
	defer heldMutex.Unlock()
	if held.Valid() && heldName == base {
		return held, nil
	}
	opened, err := pathx.OpenRoot(base)
	if err != nil {
		return pathx.Root{}, fmt.Errorf("camp keeps its records in %s, which "+
			"could not be opened: %w", base, err)
	}
	// Only a process that was pointed somewhere else mid-run gets here
	// twice -- the environment does not change under a command -- and the
	// capability it held for the old name is of no further use.
	_ = held.Close()
	held, heldName = opened, base
	return held, nil
}

// Location is where the records really land: camp's own directory below
// the state base as the kernel resolved it, rather than as the
// environment spells it.
//
// A symlinked XDG_STATE_HOME names one directory and writes into another,
// and the check that the records are not inside a repository has to be
// made against the second one.
func Location() (string, error) {
	base, err := root()
	if err != nil {
		return "", err
	}
	_, name := where()
	return filepath.Join(base.Name(), name), nil
}

// area is where camp's records are written.
func area() (fsx.Area, error) {
	base, err := root()
	if err != nil {
		return fsx.Area{}, err
	}
	_, name := where()
	return fsx.State(base, name), nil
}

// Path is one record's file.
func Path(hash string) string { return filepath.Join(Dir(), hash+".json") }

// FromPlan builds the record for a composition about to be mounted.
//
// The staging directory is a parameter rather than something filled in
// afterwards because the record has to be complete in one construction:
// it is written before the helper's first syscall, and a field added to
// it later is a field that is missing in exactly the window this record
// exists for.
func FromPlan(built plan.Plan, staging, tool, configDigest, inventoryDigest string,
	uid, gid int) Record {
	now := time.Now().UTC().Format(time.RFC3339)
	record := Record{
		Version:         Version,
		UID:             uid,
		GID:             gid,
		Config:          built.Config.Source,
		Env:             built.Config.Env,
		Live:            built.Live,
		Upper:           built.Config.UpperPath(),
		Workspace:       built.Config.LowerPath(),
		Hash:            built.Hash,
		ConfigDigest:    configDigest,
		InventoryDigest: inventoryDigest,
		Phase:           Mounting,
		Tool:            tool,
		Staging:         staging,
		// Both points the helper binds onto itself, in the order it makes
		// them: the staging point before it builds anything in it, the live
		// point before the tree is moved onto it, so that neither move can
		// propagate. Both, because the staging one is the parent of the whole
		// tree for the length of the helper's work -- recording only the live
		// one left the first mount of every composition, and the one that is
		// standing longest, in no record at all.
		//
		// Written down here rather than inferred at teardown, because a
		// teardown works from this record and from nothing else.
		Detached:  []string{staging, built.Live},
		Created:   []string{built.Work, built.OverlayWork},
		CreatedAt: now,
		UpdatedAt: now,
	}
	for _, mount := range built.Mounts {
		recorded := Mount{
			Kind:   string(mount.Kind),
			Role:   string(mount.Role),
			Source: mount.Source,
			Target: mount.Target,
			// Where it stands until the move, from the plan's own derivation
			// rather than a second one here: the helper is handed the same
			// path, and two derivations that can drift would put the record's
			// staging location somewhere nothing was ever mounted.
			Staging: mount.InStaging(staging),
		}
		// The overlay's operands, as the string the kernel was given.
		// Rendered by the same function that mounts it, so the record cannot
		// describe a mount that was made differently.
		if mount.Kind == plan.Overlay {
			recorded.FSType = "overlay"
			recorded.Options = mountx.Options(mount)
		}
		record.Mounts = append(record.Mounts, recorded)
	}
	return record
}

// Strand writes down places a rollback or a teardown could not remove.
//
// Never the same one twice: a second attempt that strands the same mount
// is the same mount, and a record that grew a line at every failed 'camp
// down' would stop being a list anybody could read. A place that has come
// down since is left in the list and costs one "absent" answer, which is
// cheaper than a record that forgot where something was.
func (r *Record) Strand(paths ...string) {
	for _, path := range paths {
		if path == "" || contains(r.Stranded, path) {
			continue
		}
		r.Stranded = append(r.Stranded, path)
	}
}

func contains(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

// Save writes the record.
//
// Temp file, rename, both file and directory synced. The directory is
// 0700 and the file 0600: it names every path of a composition, and it is
// nobody else's business.
func (r Record) Save() error {
	r.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	area, err := area()
	if err != nil {
		return err
	}
	if err := area.Ensure(0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the record: %w", err)
	}
	return area.Write(r.Hash+".json", append(data, '\n'), 0o600)
}

// Load reads one record.
//
// A record it cannot parse is an error rather than an absence: silently
// treating a corrupt record as "no composition here" would lose the only
// list of what is mounted.
func Load(hash string) (Record, bool, error) {
	data, err := os.ReadFile(Path(hash))
	if err != nil {
		if os.IsNotExist(err) {
			return Record{}, false, nil
		}
		return Record{}, false, fmt.Errorf("reading %s: %w", Path(hash), err)
	}
	record, err := Decode(data)
	if err != nil {
		return Record{}, true, fmt.Errorf("%s: %w", Path(hash), err)
	}
	// The file is named after the composition it describes. A record whose
	// hash says otherwise was moved or edited, and the two things that
	// disagree are the two things a teardown is addressed by.
	if record.Hash != hash {
		return Record{}, true, fmt.Errorf("%s describes the composition %s. The "+
			"file is named after the composition it holds, so one of the two "+
			"has been changed by hand", Path(hash), record.Hash)
	}
	return record, true, nil
}

// Decode parses a record strictly and checks that it says what a record
// has to say.
//
// Strictly, because of what this file is for: it is read when something
// has already gone wrong, and it is the only list of what is mounted. A
// field the reader does not understand is a field somebody expected it to
// honour, and a record that parses while naming no mounts would be a
// teardown that succeeds by doing nothing.
func Decode(data []byte) (Record, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("the record does not parse: %w", err)
	}
	if decoder.More() {
		return Record{}, fmt.Errorf("the record is followed by more data, and " +
			"one file is one composition")
	}
	if record.Version != Version {
		return Record{}, fmt.Errorf("the record is version %d and this camp "+
			"writes and reads version %d. It was written by a different build; "+
			"use that build to take the composition down, rather than letting "+
			"this one guess at what the fields mean", record.Version, Version)
	}
	return record, record.check()
}

// check refuses a record that cannot mean what it says.
func (r Record) check() error {
	if r.Hash == "" {
		return fmt.Errorf("the record names no composition: its hash is empty")
	}
	switch r.Phase {
	case Mounting, Up, Partial, Down:
	default:
		return fmt.Errorf("the record's phase is %q, and camp knows %q, %q, %q "+
			"and %q", r.Phase, Mounting, Up, Partial, Down)
	}
	for field, path := range map[string]string{
		"live": r.Live, "env": r.Env, "config": r.Config,
		"upper": r.Upper, "workspace": r.Workspace,
	} {
		if err := canonical(field, path); err != nil {
			return err
		}
	}
	if len(r.Mounts) == 0 {
		return fmt.Errorf("the record names no mounts. A composition always has " +
			"at least one, and a teardown that finds none would report success " +
			"for having done nothing")
	}

	if r.Staging != "" {
		if err := canonical("staging", r.Staging); err != nil {
			return err
		}
	}

	// Two paths per mount, and each of them is one mount in a teardown --
	// the same refusal twice, because the teardown names both lists and a
	// path named twice is two unmounts of one thing.
	seen, staged := map[string]bool{}, map[string]bool{}
	for index, mount := range r.Mounts {
		if mount.Kind == "" {
			return fmt.Errorf("recorded mount %d has no kind", index+1)
		}
		if err := canonical(fmt.Sprintf("recorded mount %d's target", index+1),
			mount.Target); err != nil {
			return err
		}
		if seen[mount.Target] {
			return fmt.Errorf("%s is recorded twice, and one path is one mount "+
				"in a teardown", mount.Target)
		}
		seen[mount.Target] = true

		if mount.Staging == "" {
			continue
		}
		if err := canonical(fmt.Sprintf("recorded mount %d's staging location",
			index+1), mount.Staging); err != nil {
			return err
		}
		// The staging tree is where the whole composition is built, so a
		// staging location outside it is a path this record cannot account
		// for: the helper mounts inside one directory, and a teardown that
		// named something elsewhere would be addressing a mount nobody made.
		if !pathx.Under(mount.Staging, r.Staging) {
			return fmt.Errorf("recorded mount %d stands at %s before the move, "+
				"and the staging tree is %q. Everything built before the move is "+
				"built inside it", index+1, mount.Staging, r.Staging)
		}
		if staged[mount.Staging] {
			return fmt.Errorf("%s is recorded twice as a staging location, and "+
				"one path is one mount in a teardown", mount.Staging)
		}
		staged[mount.Staging] = true
	}
	for _, path := range r.Detached {
		if err := canonical("a detached mount point", path); err != nil {
			return err
		}
	}
	for _, path := range r.Stranded {
		if err := canonical("a stranded mount", path); err != nil {
			return err
		}
	}
	for _, path := range r.Created {
		if err := canonical("a created path", path); err != nil {
			return err
		}
	}
	return nil
}

// canonical refuses a path that is not absolute and already normalised.
// Everything in a record was resolved before it was written, and a path
// with a ".." in it is a path somebody edited.
func canonical(field, path string) error {
	if path == "" {
		return fmt.Errorf("the record's %s is empty", field)
	}
	if !filepath.IsAbs(path) || path != filepath.Clean(path) {
		return fmt.Errorf("the record's %s is %q, and every path in a record is "+
			"absolute and already resolved", field, path)
	}
	return nil
}

// Listing is one entry of what list prints. A record that will not parse
// appears here too, marked, because a corrupt record is a thing to be
// told about and never something to skip.
type Listing struct {
	Path    string
	Record  Record
	Corrupt error
}

// All returns every record, newest first.
func All() []Listing {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		// No directory means nothing was ever recorded. Anything else is a
		// state directory camp cannot read, and answering "no compositions"
		// to that would be the most misleading sentence camp could say.
		if os.IsNotExist(err) {
			return nil
		}
		return []Listing{{Path: Dir(), Corrupt: err}}
	}
	var listings []Listing
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(Dir(), entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			listings = append(listings, Listing{Path: path, Corrupt: err})
			continue
		}
		record, err := Decode(data)
		if err != nil {
			listings = append(listings, Listing{Path: path, Corrupt: err})
			continue
		}
		listings = append(listings, Listing{Path: path, Record: record})
	}
	sort.Slice(listings, func(i, j int) bool {
		return listings[i].Record.UpdatedAt > listings[j].Record.UpdatedAt
	})
	return listings
}

// Forget removes one record and nothing else. Not a repository, not the
// storage, not the composed tree -- one file.
//
// Whether a record may go at all is Release's question, and every command
// asks it there.
func Forget(hash string) error {
	area, err := area()
	if err != nil {
		return err
	}
	return area.Remove(hash + ".json")
}

// Presence is what the machine says about one recorded mount.
//
// Path and identity, never the path alone: a mount that went away and a
// mount somebody else made at the same path look identical to a scan by
// name, and unmounting the second one because the first is written down
// here would remove a stranger's mount.
type Presence string

const (
	// Gone: nothing is mounted at that path.
	Gone Presence = "gone"
	// Same: what is mounted there is the object camp mounted.
	Same Presence = "same"
	// Different: something is mounted there and it is not camp's.
	Different Presence = "different"
	// Unverified: something is mounted there and the record cannot say
	// whether it is the same object -- either the identity was never
	// written down, because the record predates the mount, or the path
	// cannot be looked at now.
	Unverified Presence = "unverified"
)

// Presence answers for one recorded mount where it ends up.
func (m Mount) Presence(table []mountinfo.Entry) (Presence, error) {
	return m.PresenceAt(m.Target, table)
}

// PresenceAt answers for one recorded mount at one of the two places it
// can be.
//
// The identity is the same at both: a mount moved from staging to the
// live path is the same mount, and what it put there answers with the
// same device and inode wherever the mount hangs. So a mount found in
// staging can be told from a stranger's exactly as well as one found at
// its target.
func (m Mount) PresenceAt(path string, table []mountinfo.Entry) (Presence, error) {
	if path == "" || len(mountinfo.At(table, path)) == 0 {
		return Gone, nil
	}
	if m.Device == 0 && m.Inode == 0 {
		return Unverified, nil
	}
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return Unverified, fmt.Errorf("looking at %s: %w", path, err)
	}
	if uint64(st.Dev) != m.Device || st.Ino != m.Inode {
		return Different, nil
	}
	return Same, nil
}

// Digest is how the record fingerprints a file it wants to notice the
// editing of. Empty bytes have no digest: a file that was not read is not
// a file that was empty.
func Digest(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Place is one place a teardown has to look: a path, and the identity
// camp recorded for what it put there.
//
// Zero identity means the record has none for this place -- it was
// written before the mount was made, or the place is a self-bind, which
// was covered by the composition for its whole life and could never be
// looked at. The helper reads that as authority to unmount what it was
// given, because refusing would leave somebody walled in behind mounts
// camp made.
type Place struct {
	Path   string
	Device uint64
	Inode  uint64
}

// Teardown returns every place this composition may have a mount, in the
// order they come apart.
//
// The helper builds in one order: the staging point bound onto itself,
// the mounts inside it, the live point bound onto itself, the move, and
// then the staging self-bind comes off. A kill lands anywhere in that
// window and the record cannot know where, so it names both places for
// every mount -- the live targets in reverse, then the staging targets in
// reverse, then the two self-binds in the reverse of the order they were
// made.
//
// Live before staging, and both before either self-bind, because that is
// the order that takes a tree apart from the top whichever state the
// machine is in. A mount is only ever in one of its two places, and
// naming the empty one costs one "absent" answer: the helper compares
// identity before it unmounts, and a path with nothing at it produces no
// mismatch and nothing to remove. What must not happen is the other way
// round -- a mount standing at a place no target names.
func (r Record) Teardown() []Place {
	order := make([]Place, 0, 2*len(r.Mounts)+len(r.Detached))
	for i := len(r.Mounts) - 1; i >= 0; i-- {
		mount := r.Mounts[i]
		order = append(order, Place{
			Path: mount.Target, Device: mount.Device, Inode: mount.Inode})
	}
	for i := len(r.Mounts) - 1; i >= 0; i-- {
		mount := r.Mounts[i]
		if mount.Staging == "" {
			continue
		}
		order = append(order, Place{
			Path: mount.Staging, Device: mount.Device, Inode: mount.Inode})
	}
	for i := len(r.Detached) - 1; i >= 0; i-- {
		order = append(order, Place{Path: r.Detached[i]})
	}

	// A stranded location the plan already names keeps the place the order
	// above gives it; one it does not name -- a mount that appeared where
	// nothing planned one -- goes first, and deepest first, because nothing
	// here knows what stands on what and a parent cannot come off before
	// its children.
	named := map[string]bool{}
	for _, location := range order {
		named[location.Path] = true
	}
	var extra []Place
	for _, path := range r.Stranded {
		if path == "" || named[path] {
			continue
		}
		named[path] = true
		extra = append(extra, Place{Path: path})
	}
	sort.SliceStable(extra, func(i, j int) bool {
		return strings.Count(extra[i].Path, "/") > strings.Count(extra[j].Path, "/")
	})
	return append(extra, order...)
}

// Held returns what is on the machine that this record answers for: a
// mount at any of the places a teardown would name, and any mount at or
// beneath the three areas the composition owns -- its work directory, its
// staging tree and its live tree.
//
// At for the recorded places, beneath for the three areas, and the
// difference is deliberate. A recorded target may be a directory that has
// mounts of somebody else's inside it -- the workspace is one, and a
// record that could never be discarded because of an unrelated mount
// under it would be its own trap. The three areas are camp's own, so a
// mount anywhere in one of them is camp's business, and one that no plan
// names is exactly the thing a record must not be discarded over.
func Held(record Record, table []mountinfo.Entry) []string {
	var standing []string
	seen := map[string]bool{}
	add := func(found []mountinfo.Entry) {
		for _, entry := range found {
			if entry.Point == "" || seen[entry.Point] {
				continue
			}
			seen[entry.Point] = true
			standing = append(standing, entry.Point)
		}
	}
	for _, place := range record.Teardown() {
		add(mountinfo.At(table, place.Path))
	}
	for _, area := range append([]string{record.Staging, record.Live}, record.Created...) {
		if area == "" {
			continue
		}
		add(mountinfo.Under(table, area))
	}
	return standing
}

// Release discards a record, unless something it answers for is still on
// the machine -- in which case it keeps the record, writes down what is
// standing, and says what and why.
//
// The one place that decision is made, and every command that discards a
// record goes through it. A record is the only list of what a composition
// put where, so discarding it while any of that is standing leaves those
// mounts with nothing that knows about them, and on a machine-wide mount
// table that is somebody walled in with no way back. The kernel's table
// decides it and not the phase, which a crash can leave saying anything.
//
// What it finds standing goes into the record as stranded places rather
// than into the message alone: a mount in one of camp's areas that no
// plan names would otherwise be a thing camp could describe once and
// never address again.
func Release(record Record, table []mountinfo.Entry) error {
	held := Held(record, table)
	if len(held) == 0 {
		return Forget(record.Hash)
	}

	record.Strand(held...)
	record.Phase = Partial
	kept := ""
	if err := record.Save(); err != nil {
		kept = fmt.Sprintf("\nThe record could not be written either (%v), so "+
			"it still says what it said before this run.", err)
	}
	return fmt.Errorf("the record %s is kept: %d mount(s) it answers for are "+
		"still on the machine: %s.\n"+
		"It is the only list of what this composition put where, so camp does "+
		"not discard it while any of that is standing -- there would be nothing "+
		"left that knows those mounts are camp's. 'camp status' says what is "+
		"there and 'camp down' removes what camp made; the record goes when the "+
		"last of it does.%s",
		record.Hash, len(held), strings.Join(held, ", "), kept)
}

// Age renders how long ago the record was last written.
func (r Record) Age() string {
	updated, err := time.Parse(time.RFC3339, r.UpdatedAt)
	if err != nil {
		return "unknown"
	}
	elapsed := time.Since(updated).Round(time.Second)
	switch {
	case elapsed < time.Minute:
		return fmt.Sprintf("%ds ago", int(elapsed.Seconds()))
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(elapsed.Hours()/24))
	}
}
