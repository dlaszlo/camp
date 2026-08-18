// Package islands builds camp's third mount type: the source's
// contributed entries standing read-only in a writable, machine-local
// floor.
//
// The picture the name comes from: the *islands* are what a repository
// contributes and they are read-only; the *water* is camp's own storage,
// which covers the whole target and takes every write. A runtime file
// that exists in no repository -- settings.local.json, a lock file, a
// worktree -- lands in the water, is machine-local, and survives the
// session. Editing a contributed entry through the composed tree is
// EROFS.
//
// This shape is not a preference; it is the only legal one for a
// directory that is part repository and part machine state. A file like
// settings.local.json exists in no repository, so a plain writable hole
// has nothing to bind onto -- a bind cannot create its own mount point --
// and creating the attachment point through the overlay would copy the
// whole directory up into the code repository, which is forbidden. Only a
// store covering the entire directory can provide attachment points
// without touching a repository.
//
// One alternative was rejected on mechanism and is recorded so it is not
// relitigated: making the directory its own small overlay with camp's
// storage as the upper. There, editing a *tracked* entry copies it up
// silently into scratch storage -- the change looks applied and exists in
// no repository, which is this design's worst failure shape. The islands
// form is precise instead: what is writable is the water, and everything
// contributed fails loudly.
package islands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dlaszlo/camp/internal/enc"
	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/refusal"
)

// ManifestName records the attachment points camp created in the water.
//
// It sits beside the target marker, outside every store subtree, and it
// exists because those objects persist: on the next up they are already
// in the water, and the collision rule -- which refuses to shadow the
// user's machine-local content -- would otherwise refuse camp's own
// scaffolding.
const ManifestName = ".camp-scaffold"

// Reserved are the names camp keeps for itself inside a storage
// directory. A mount target with one of these names would have its store
// land exactly on top of one of them.
//
// The specification does not raise this case -- it assumes the manifest
// sits outside every store -- so camp refuses the collision rather than
// deciding quietly which of the two wins.
var Reserved = []string{ManifestName, ".camp-target"}

// Entry is one thing a source contributes.
type Entry struct {
	Name string
	Type pathx.Type
}

// Expansion is one islands mount, made concrete.
type Expansion struct {
	// Target is the mount point, relative to the merged root.
	Target pathx.Rel
	// Store is the writable floor's absolute path in camp's storage.
	Store string
	// Source is the directory whose contributed entries become islands.
	Source string
	// SourceParts is the same path as components beneath the environment
	// root.
	SourceParts []string
	// Entries are the islands, in byte order.
	Entries []Entry
}

// Manifest is the record of what camp created in the water.
type Manifest struct {
	area    fsx.Area
	entries map[string]pathx.Type
}

// LoadManifest reads the manifest for a storage directory.
func LoadManifest(storage fsx.Area) (*Manifest, error) {
	manifest := &Manifest{area: storage, entries: map[string]pathx.Type{}}

	path, err := storage.Path(ManifestName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return manifest, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	records, err := enc.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for number, record := range records {
		if len(record) != 2 {
			return nil, fmt.Errorf("%s line %d has %d fields and a record has "+
				"two: a type and a store-relative path", path, number+1, len(record))
		}
		manifest.entries[record[1]] = pathx.Type(record[0])
	}
	return manifest, nil
}

// Records reports whether the manifest knows about a path, and as what.
func (m *Manifest) Records(relative string) (pathx.Type, bool) {
	kind, known := m.entries[relative]
	return kind, known
}

// Add records an attachment point and writes the manifest **before** the
// object is created.
//
// Write-ahead, deliberately. A crash between the record and the creation
// leaves at worst a recorded name with nothing on disk, which the next up
// simply creates -- harmless. The other order would leave an object in
// the user's storage that camp could not prove was its own, and the
// collision rule would then refuse the composition on the strength of
// camp's own scaffolding.
func (m *Manifest) Add(relative string, kind pathx.Type) error {
	if existing, known := m.entries[relative]; known && existing == kind {
		return nil
	}
	// Saved first, remembered second. What is on disk is the provenance;
	// what is in this map is a copy of it, and a copy that ran ahead of a
	// failed write would have camp believe it owns something no record
	// claims.
	if err := m.saveWith(relative, kind); err != nil {
		return err
	}
	m.entries[relative] = kind
	return nil
}

// Remove strikes an entry from the manifest.
func (m *Manifest) Remove(relative string) error {
	if _, known := m.entries[relative]; !known {
		return nil
	}
	if err := m.saveWithout(relative); err != nil {
		return err
	}
	delete(m.entries, relative)
	return nil
}

// saveWith and saveWithout write the manifest as it would be with one
// entry added or removed, without changing what this process believes
// until the write has landed.
func (m *Manifest) saveWith(relative string, kind pathx.Type) error {
	next := make(map[string]pathx.Type, len(m.entries)+1)
	for name, existing := range m.entries {
		next[name] = existing
	}
	next[relative] = kind
	return m.write(next)
}

func (m *Manifest) saveWithout(relative string) error {
	next := make(map[string]pathx.Type, len(m.entries))
	for name, existing := range m.entries {
		if name != relative {
			next[name] = existing
		}
	}
	return m.write(next)
}

func (m *Manifest) write(entries map[string]pathx.Type) error {
	lines := make([]string, 0, len(entries))
	for relative, kind := range entries {
		lines = append(lines, enc.Line(string(kind), relative))
	}
	enc.Sort(lines)
	return m.area.Write(ManifestName, enc.Document(lines), 0o644)
}

// Scaffold creates the attachment points one islands mount needs, and
// refuses rather than shadowing anything of the user's.
//
// The rule is one rule for file and directory islands alike. A needed
// attachment point that already exists is accepted only if the manifest
// records it **and** it is unchanged -- a zero-length file, an empty
// directory. Recorded but modified is refused, with both sides named,
// because mounting would hide what is now the user's content. Present but
// unrecorded is refused the same way: camp cannot prove it is not the
// user's.
func Scaffold(storage fsx.Area, manifest *Manifest, expansion Expansion) (refusal.List, []string) {
	var refused refusal.List

	// The water itself: camp's own storage, created here rather than
	// assumed, because the attachment points below it have nowhere to go
	// until it exists.
	if err := storage.Ensure(0o755); err != nil {
		refused.Add("islands-store", "%v", err)
		return refused, nil
	}
	if _, err := storage.MkdirDeep(expansion.Target.Components()); err != nil {
		refused.Add("islands-store", "%v", err)
		return refused, nil
	}
	area, err := storage.Sub(expansion.Target.Components()...)
	if err != nil {
		refused.Add("islands-store", "%v", err)
		return refused, nil
	}

	for _, entry := range expansion.Entries {
		relative := strings.Join(append(expansion.Target.Components(), entry.Name), "/")
		path := filepath.Join(expansion.Store, entry.Name)

		info, err := pathx.StatBeneath(expansion.Store, []string{entry.Name})
		if err != nil {
			refused.Add("islands-store", "looking at %s: %v", path, err)
			continue
		}

		if info.Exists() {
			refused.Extend(accept(manifest, relative, path, entry, info))
			continue
		}

		// Write-ahead: record first, create second.
		if err := manifest.Add(relative, entry.Type); err != nil {
			refused.Add("islands-manifest", "%v", err)
			continue
		}
		switch entry.Type {
		case pathx.Dir:
			if _, err := area.MkdirAll(entry.Name); err != nil {
				refused.Add("islands-store", "%v", err)
			}
		case pathx.File:
			if _, _, err := area.Touch(entry.Name); err != nil {
				refused.Add("islands-store", "%v", err)
			}
		default:
			refused.Add("islands-entry-type",
				"the source contributes %q at %q as a %s, and camp can stand a "+
					"directory or a regular file in the water and nothing else.",
				entry.Name, expansion.Target.String(), entry.Type)
		}
	}

	notes := retire(storage, manifest, expansion)
	return refused, notes
}

// accept decides whether an attachment point that is already there may be
// used again.
func accept(manifest *Manifest, relative, path string, entry Entry, info pathx.Info) refusal.List {
	var refused refusal.List

	recorded, known := manifest.Records(relative)
	if !known {
		refused.Add("islands-collision",
			"%s already holds %q, and camp did not put it there.\n"+
				"  in the water: %s (%s)\n"+
				"  the source now contributes: %s (%s)\n"+
				"Mounting the island would hide your file behind the "+
				"repository's. camp will not do that silently, and it will not "+
				"delete your file either -- that is your move. Rename it or "+
				"remove it, and run this again.",
			filepath.Dir(path), entry.Name, path, info.Type, entry.Name, entry.Type)
		return refused
	}
	if recorded != entry.Type || info.Type != entry.Type {
		refused.Add("islands-collision",
			"%s is a %s and the island needs a %s.\n"+
				"camp recorded it as a %s when it created it. Remove it and run "+
				"this again.", path, info.Type, entry.Type, recorded)
		return refused
	}

	unchanged, err := untouched(path, entry.Type)
	if err != nil {
		refused.Add("islands-store", "looking at %s: %v", path, err)
		return refused
	}
	if !unchanged {
		refused.Add("islands-scaffold-modified",
			"%s is camp's own attachment point and it is no longer empty.\n"+
				"It was created so that the repository's %q could be mounted over "+
				"it, and something has written to it since. Mounting the island "+
				"now would hide that content. Move it somewhere else, and run "+
				"this again.", path, entry.Name)
	}
	return refused
}

// untouched reports whether an attachment point is still what camp made:
// a zero-length file, or an empty directory.
func untouched(path string, kind pathx.Type) (bool, error) {
	if kind == pathx.File {
		info, err := os.Lstat(path)
		if err != nil {
			return false, err
		}
		return info.Size() == 0, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// retire deals with an island that has disappeared from the source.
//
// A still-empty recorded scaffold is removed and struck from the
// manifest: deleting camp's own object is exactly what the invariant
// permits. A modified one is left where it is, struck from the manifest
// and reported -- it has become ordinary water content, which is the
// user's.
//
// The order and the errors are the whole of the care here, because what
// is at stake is provenance. The object goes first and the record after
// it: a crash in between leaves a record for something that is not there,
// which the next run simply creates again, while the other order would
// leave an object in the user's storage that camp could no longer prove
// was its own -- and the collision rule would then refuse the composition
// on the strength of camp's own scaffolding. An error that is not "it is
// gone" is never read as "it is gone" either: a permission or an I/O
// failure used to make camp disclaim an object that still exists.
func retire(storage fsx.Area, manifest *Manifest, expansion Expansion) []string {
	var notes []string

	needed := map[string]bool{}
	for _, entry := range expansion.Entries {
		needed[strings.Join(append(expansion.Target.Components(), entry.Name), "/")] = true
	}

	prefix := strings.Join(expansion.Target.Components(), "/") + "/"
	for relative, kind := range manifest.entries {
		if needed[relative] || !strings.HasPrefix(relative, prefix) {
			continue
		}
		name := strings.TrimPrefix(relative, prefix)
		if strings.Contains(name, "/") {
			continue
		}
		path := filepath.Join(expansion.Store, name)

		unchanged, err := untouched(path, kind)
		switch {
		case os.IsNotExist(err):
			// It is gone already, so only the record is left to strike.
			if err := manifest.Remove(relative); err != nil {
				notes = append(notes, fmt.Sprintf(
					"%s is gone and camp could not strike it from its own record: "+
						"%v. The record is what tells camp's objects from yours, so "+
						"it is kept rather than half-written.", path, err))
			}
		case err != nil:
			// Something else: a permission, an I/O error. camp does not know
			// what is there, so it keeps claiming it and says so.
			notes = append(notes, fmt.Sprintf(
				"%s was camp's attachment point for an entry the source no longer "+
					"contributes, and camp could not look at it: %v. It stays in "+
					"camp's record until it can -- disclaiming something that may "+
					"still be there would leave an object nothing can account for.",
				path, err))
		case !unchanged:
			if err := manifest.Remove(relative); err != nil {
				notes = append(notes, fmt.Sprintf(
					"%s is no longer camp's and camp could not strike it from its "+
						"own record: %v.", path, err))
				continue
			}
			notes = append(notes, fmt.Sprintf(
				"%s was camp's attachment point for an entry the source no "+
					"longer contributes, and it is no longer empty -- so it is now "+
					"ordinary content of yours in machine-local storage. camp has "+
					"stopped claiming it and left it exactly where it is.", path))
		default:
			// The object first, the record second: a crash in between leaves a
			// record for something that is not there, which the next run
			// creates again. Removed through the storage area and by component
			// -- the same route it was created by -- so camp's own object is
			// the only thing this can reach.
			if err := storage.Remove(append(expansion.Target.Components(), name)...); err != nil {
				notes = append(notes, fmt.Sprintf(
					"%s is camp's own attachment point for an entry the source no "+
						"longer contributes, and it could not be removed: %v. It "+
						"stays in camp's record.", path, err))
				continue
			}
			if err := manifest.Remove(relative); err != nil {
				notes = append(notes, fmt.Sprintf(
					"%s was removed and camp could not strike it from its own "+
						"record: %v. The next run recreates it and strikes it "+
						"again.", path, err))
			}
		}
	}
	return notes
}
