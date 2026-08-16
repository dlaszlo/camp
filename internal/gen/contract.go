package gen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dlaszlo/camp/internal/enc"
	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/islands"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
)

// The generation step's contract on disk.
//
// camp materialises the inputs, the step writes the outputs, and camp
// then validates what it finds as hostile data. Everything is addressed
// under the composition's work directory, which is camp's own scratch:
// the step's working directory is set there too, so that a naive
// generator's relative writes land in camp's scratch and never in a
// repository.
const (
	// InDir holds what camp gives the step.
	InDir = "in"
	// OutDir holds what the step gives back.
	OutDir = "out"

	// LowerRootList is the workspace's root entries: <type> TAB <name>.
	LowerRootList = "lower-root.list"
	// MountTargetsList is every mount target, one encoded field per line.
	MountTargetsList = "mount-targets.list"
	// AllowOverlapList is the allow_overlap set, one encoded field per line.
	AllowOverlapList = "allow-overlap.list"
	// UpperExcludeCurrent is the code repository's own exclude, raw.
	UpperExcludeCurrent = "upper-exclude.current"
	// IslandsDir holds one file per islands mount, on each side.
	IslandsDir = "islands"
	// ExcludeOut is the complete payload to mount.
	ExcludeOut = "exclude"
)

// SourceSuffix and ListSuffix name the per-target files.
const (
	SourceSuffix = ".source"
	ListSuffix   = ".list"
)

// Paths returns the directories of one generation run.
type Paths struct {
	Root string
	In   string
	Out  string
}

// PathsFor returns them for a composition.
func PathsFor(built plan.Plan) Paths {
	root := built.GenDir()
	return Paths{Root: root, In: filepath.Join(root, InDir), Out: filepath.Join(root, OutDir)}
}

// WriteInputs materialises everything the step is given.
//
// Every list uses the one encoding camp writes every record with, so a
// name containing a tab, a newline or bytes that are not valid UTF-8
// survives the trip in both directions.
func WriteInputs(built plan.Plan, existing []byte) refusal.List {
	var refused refusal.List

	work := fsx.Work(built.Work)
	gen, err := work.Sub("gen")
	if err != nil {
		refused.Add("generate-scratch", "%v", err)
		return refused
	}
	if _, err := work.MkdirAll("gen"); err != nil {
		refused.Add("generate-scratch", "%v", err)
		return refused
	}
	in, err := gen.Sub(InDir)
	if err != nil {
		refused.Add("generate-scratch", "%v", err)
		return refused
	}
	if _, err := gen.MkdirAll(InDir); err != nil {
		refused.Add("generate-scratch", "%v", err)
		return refused
	}
	if _, err := gen.MkdirAll(OutDir); err != nil {
		refused.Add("generate-scratch", "%v", err)
		return refused
	}

	var roots []string
	for _, entry := range built.LowerRoot {
		roots = append(roots, enc.Line(string(entry.Type), entry.Name))
	}
	enc.Sort(roots)

	var targets []string
	for _, mount := range built.Mounts {
		if mount.InLive && !mount.Rel.Empty() {
			targets = append(targets, enc.Line(mount.Rel.String()))
		}
	}
	enc.Sort(targets)

	var allowed []string
	for _, name := range built.Config.AllowOverlap {
		allowed = append(allowed, enc.Line(name))
	}
	enc.Sort(allowed)

	writes := []struct {
		name string
		data []byte
	}{
		{LowerRootList, enc.Document(roots)},
		{MountTargetsList, enc.Document(targets)},
		{AllowOverlapList, enc.Document(allowed)},
		{UpperExcludeCurrent, existing},
	}
	for _, write := range writes {
		if err := in.Write(write.name, write.data, 0o644); err != nil {
			refused.Add("generate-scratch", "%v", err)
		}
	}

	if len(built.IslandsMounts) == 0 {
		return refused
	}
	if _, err := in.MkdirAll(IslandsDir); err != nil {
		refused.Add("generate-scratch", "%v", err)
		return refused
	}
	islandsIn, err := in.Sub(IslandsDir)
	if err != nil {
		refused.Add("generate-scratch", "%v", err)
		return refused
	}
	for _, mount := range built.IslandsMounts {
		area, name, err := nested(islandsIn, mount.Target.Components())
		if err != nil {
			refused.Add("generate-scratch", "%v", err)
			continue
		}
		if err := area.Write(name+SourceSuffix, []byte(mount.Source+"\n"), 0o644); err != nil {
			refused.Add("generate-scratch", "%v", err)
		}
	}
	return refused
}

// nested walks a target's components into an area, creating the
// directories above the last one, so that a deep target's per-target file
// mirrors the target's own path.
func nested(area fsx.Area, components []string) (fsx.Area, string, error) {
	for index := 0; index < len(components)-1; index++ {
		if _, err := area.MkdirAll(components[index]); err != nil {
			return fsx.Area{}, "", err
		}
		next, err := area.Sub(components[index])
		if err != nil {
			return fsx.Area{}, "", err
		}
		area = next
	}
	return area, components[len(components)-1], nil
}

// ReadOutputs reads back what the step produced.
func ReadOutputs(built plan.Plan) (Output, refusal.List) {
	var refused refusal.List
	var out Output

	paths := PathsFor(built)
	payload, err := os.ReadFile(filepath.Join(paths.Out, ExcludeOut))
	if err != nil {
		refused.Add("generate-no-exclude",
			"the generation step produced no %s: %v.\n"+
				"The step's contract is to write the complete exclude payload -- "+
				"the repository's own bytes and camp's block -- to $CAMP_GEN_OUT/%s.",
			ExcludeOut, err, ExcludeOut)
		return out, refused
	}
	out.Exclude = payload

	out.Islands = map[string][]islands.Entry{}
	for _, mount := range built.IslandsMounts {
		path := filepath.Join(append([]string{paths.Out, IslandsDir},
			mount.Target.Components()...)...) + ListSuffix
		data, err := os.ReadFile(path)
		if err != nil {
			refused.Add("generate-no-islands",
				"the generation step produced no list for the islands mount at "+
					"%q: %v.\nIt has to write $CAMP_GEN_OUT/%s/%s%s, one record per "+
					"line: <type> TAB <entry>.",
				mount.Target.String(), err, IslandsDir, mount.Target.String(), ListSuffix)
			continue
		}
		records, err := enc.Parse(data)
		if err != nil {
			refused.Add("generate-islands-undecodable",
				"the islands list for %q does not decode: %v.\n"+
					"Every record camp reads or writes uses one escaping -- \\\\ for "+
					"a backslash, \\t for a tab, \\n for a newline, \\xHH for the "+
					"remaining control bytes -- so that a name can hold anything a "+
					"Linux name can and the framing is still unambiguous.",
				mount.Target.String(), err)
			continue
		}
		var entries []islands.Entry
		for number, record := range records {
			if len(record) != 2 {
				refused.Add("generate-islands-shape",
					"record %d of the islands list for %q has %d fields; a record "+
						"is <type> TAB <entry>.", number+1, mount.Target.String(), len(record))
				continue
			}
			entries = append(entries, islands.Entry{
				Type: pathx.Type(record[0]),
				Name: record[1],
			})
		}
		out.Islands[mount.Target.String()] = entries
	}
	return out, refused
}

// WriteOutputs is how the built-in step publishes what it produced, using
// exactly the contract an external one would.
//
// Same door for both, so the shipped generator cannot quietly rely on a
// shortcut an external one does not have.
func WriteOutputs(built plan.Plan, out Output) refusal.List {
	var refused refusal.List

	work := fsx.Work(built.Work)
	gen, err := work.Sub("gen")
	if err != nil {
		refused.Add("generate-scratch", "%v", err)
		return refused
	}
	outArea, err := gen.Sub(OutDir)
	if err != nil {
		refused.Add("generate-scratch", "%v", err)
		return refused
	}
	if err := outArea.Write(ExcludeOut, out.Exclude, 0o644); err != nil {
		refused.Add("generate-scratch", "%v", err)
	}

	if len(out.Islands) == 0 {
		return refused
	}
	if _, err := outArea.MkdirAll(IslandsDir); err != nil {
		refused.Add("generate-scratch", "%v", err)
		return refused
	}
	islandsOut, err := outArea.Sub(IslandsDir)
	if err != nil {
		refused.Add("generate-scratch", "%v", err)
		return refused
	}

	for _, mount := range built.IslandsMounts {
		entries := out.Islands[mount.Target.String()]
		lines := make([]string, 0, len(entries))
		for _, entry := range entries {
			lines = append(lines, enc.Line(string(entry.Type), entry.Name))
		}
		enc.Sort(lines)

		area, name, err := nested(islandsOut, mount.Target.Components())
		if err != nil {
			refused.Add("generate-scratch", "%v", err)
			continue
		}
		if err := area.Write(name+ListSuffix, enc.Document(lines), 0o644); err != nil {
			refused.Add("generate-scratch", "%v", err)
		}
	}
	return refused
}

// Describe renders the expansion for plan, so that what will be mounted
// can be read before it is.
func Describe(built plan.Plan, out Output) string {
	if len(built.IslandsMounts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, mount := range built.IslandsMounts {
		entries := out.Islands[mount.Target.String()]
		fmt.Fprintf(&b, "  %s: a writable floor at %s, with %d island(s) from %s\n",
			mount.Target.String(), mount.Store, len(entries), mount.Source)
		for _, entry := range entries {
			fmt.Fprintf(&b, "      %-9s %s\n", entry.Type, entry.Name)
		}
	}
	return b.String()
}
