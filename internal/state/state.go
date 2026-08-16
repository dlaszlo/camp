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
// It is written and read **only by the unprivileged front end**. sudo
// wraps the helper alone, so XDG_STATE_HOME and the home directory always
// resolve in the invoking user's environment, and the
// root-home-versus-user-home ambiguity cannot arise.
//
// The namespace mode has no record at all and needs none: the namespace
// is the state, and it vanishes with its last process.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/mountinfo"
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

	// Created is every path camp made: the work directory, the scaffold
	// entries. It is the entire list camp is allowed to remove.
	Created []string `json:"created"`

	// Staging is where the tree was built before the move, in the
	// privileged mode.
	Staging string `json:"staging,omitempty"`

	// Detached are the mount points the privileged helper bound onto
	// themselves so that what was moved onto them could not propagate.
	// They sit underneath the composition and come off after it, so a
	// teardown removes them last.
	Detached []string `json:"detached,omitempty"`

	Phase Phase `json:"phase"`

	Tool      string `json:"tool_version"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Dir is where records live: the invoking user's state directory, always
// resolved in the invoking user's environment.
func Dir() string {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "camp")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "camp")
	}
	return filepath.Join(home, ".local", "state", "camp")
}

// Path is one record's file.
func Path(hash string) string { return filepath.Join(Dir(), hash+".json") }

// FromPlan builds the record for a composition about to be mounted.
func FromPlan(built plan.Plan, tool, configDigest, inventoryDigest string, uid, gid int) Record {
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
		// The live path is bound onto itself before the composition is moved
		// onto it, so that the move cannot propagate. It is written down
		// here rather than inferred at teardown, because a teardown works
		// from this record and from nothing else.
		Detached:  []string{built.Live},
		CreatedAt: now,
		UpdatedAt: now,
	}
	for _, mount := range built.Mounts {
		recorded := Mount{
			Kind:   string(mount.Kind),
			Role:   string(mount.Role),
			Source: mount.Source,
			Target: mount.Target,
		}
		if mount.Kind == plan.Overlay {
			recorded.FSType = "overlay"
		}
		record.Mounts = append(record.Mounts, recorded)
	}
	return record
}

// Save writes the record.
//
// Temp file, rename, both file and directory synced. The directory is
// 0700 and the file 0600: it names every path of a composition, and it is
// nobody else's business.
func (r Record) Save() error {
	r.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	area := fsx.State(Dir())
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
	return record, true, nil
}

// Decode parses a record and refuses a version it does not know.
func Decode(data []byte) (Record, error) {
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, fmt.Errorf("the record does not parse: %w", err)
	}
	if record.Version != Version {
		return Record{}, fmt.Errorf("the record is version %d and this camp "+
			"writes and reads version %d. It was written by a different build; "+
			"use that build to take the composition down, rather than letting "+
			"this one guess at what the fields mean", record.Version, Version)
	}
	return record, nil
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
		return nil
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
func Forget(hash string) error {
	return fsx.State(Dir()).Remove(hash + ".json")
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

// Presence answers for one recorded mount.
func (m Mount) Presence(table []mountinfo.Entry) (Presence, error) {
	if len(mountinfo.At(table, m.Target)) == 0 {
		return Gone, nil
	}
	if m.Device == 0 && m.Inode == 0 {
		return Unverified, nil
	}
	var st unix.Stat_t
	if err := unix.Stat(m.Target, &st); err != nil {
		return Unverified, fmt.Errorf("looking at %s: %w", m.Target, err)
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

// StillMounted returns the recorded mounts that are still present.
//
// This is what stops `forget` from discarding a composition that is up.
// The record is the only authoritative list of what a teardown has to
// remove -- it is down's to consume, not forget's to lose -- so the check
// is against the kernel's table and not against the phase, which a crash
// can leave behind.
func StillMounted(record Record, table []mountinfo.Entry) []string {
	var present []string
	for _, mount := range record.Mounts {
		if len(mountinfo.At(table, mount.Target)) > 0 {
			present = append(present, mount.Target)
		}
	}
	return present
}

// Teardown returns the recorded mounts in the order they come down.
func (r Record) Teardown() []Mount {
	reversed := make([]Mount, 0, len(r.Mounts))
	for i := len(r.Mounts) - 1; i >= 0; i-- {
		reversed = append(reversed, r.Mounts[i])
	}
	return reversed
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
