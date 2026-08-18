package gen

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/enc"
	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/gitwire"
	"github.com/dlaszlo/camp/internal/islands"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
)

// Output is everything the prepare phase produced.
type Output struct {
	// Exclude is the payload to mount, complete: the code repository's own
	// bytes plus camp's block. Empty when no step generates one, and then
	// nothing checks for one either -- a composition is not held to a
	// promise it never made.
	Exclude []byte
	// Patterns are camp's own lines, for plan to print.
	Patterns []string
	// ExcludeFile is where the payload was written.
	ExcludeFile string
	// Islands is one entry list per islands mount, keyed by target.
	Islands map[string][]islands.Entry
	// Notes are things worth saying that do not stop anything: a scaffold
	// camp has stopped claiming, a source that is not a git repository.
	Notes []string
}

func excludePath(built plan.Plan) string {
	return filepath.Join(built.Config.UpperPath(), ".git", "info", "exclude")
}

// expected is camp's own answer to what the generation step must produce.
//
// Every entry point below needs exactly this and nothing else: plan
// prints it, the launcher publishes it, and the process that mounts
// compares the step's real output against it. Computing it in one place
// is what makes those three the same answer.
func expected(built plan.Plan) (Output, refusal.List) {
	existing, refused := ReadExisting(excludePath(built))
	if !refused.Empty() {
		return Output{}, refused
	}
	out, problems := git(built, existing)
	refused.Extend(problems)
	out.ExcludeFile = built.ExcludeFile()
	return out, refused
}

// Prepare runs the generation step and validates everything it produced.
//
// The whole phase, in the order the specification fixes: materialise the
// inputs, run the step -- the shipped git one, or a configured command --
// read the outputs back, check them as hostile data, re-run the order and
// tracked-content rules over the mounts that now exist, and only then
// create anything in storage.
//
// It runs before anything is mounted, always as the invoking user.
func Prepare(built plan.Plan) (Output, refusal.List) {
	var refused refusal.List
	var out Output

	step, has := built.Config.GenerationStep()
	switch {
	case !has:
		// No generation step: no exclude, and the islands come from the raw
		// listing. What follows is the same for both branches, and that is
		// the point -- this one used to return here, so a composition
		// without a step got no expanded checks and no attachment points at
		// all, and its first file island had nothing to bind onto.
		out, refused = withoutAStep(built)
		if !refused.Empty() {
			return out, refused
		}

	default:
		refused.Extend(WriteInputs(built))
		if !refused.Empty() {
			return out, refused
		}

		if step.Kind == config.Generate {
			refused.Extend(external(built, step))
			if !refused.Empty() {
				return out, refused
			}
		}

		out, refused = Adopt(built)
		if !refused.Empty() {
			return out, refused
		}
		if step.Kind == config.GitExclude {
			// The shipped step publishes through the same contract an external
			// one uses, so it cannot quietly rely on a shortcut.
			refused.Extend(WriteOutputs(built, out))
			if !refused.Empty() {
				return out, refused
			}
		}
	}

	refused.Extend(expandedChecks(built, out))
	if !refused.Empty() {
		return out, refused
	}

	refused.Extend(scaffold(built, out, &out.Notes))
	if !refused.Empty() {
		return out, refused
	}

	if has {
		if err := fsx.Work(built.Config.Root, built.Hash).Write("exclude", out.Exclude, 0o644); err != nil {
			refused.Add("generate-write", "%v", err)
		}
	}
	return out, refused
}

// Preview derives what the shipped step would produce, running nothing
// and writing nothing.
//
// This is what plan prints. A configured external generator is not run
// here -- plan executes nothing, which is the whole point of it -- so for
// such a configuration the islands shown are what camp's own reading of
// git says, and the report says as much.
func Preview(built plan.Plan) (Output, refusal.List) {
	step, has := built.Config.GenerationStep()
	if !has {
		return withoutAStep(built)
	}

	out, refused := expected(built)
	if step.Kind == config.Generate {
		out.Notes = append(out.Notes,
			"this configuration runs its own generator ("+step.Command[0]+"), "+
				"and plan does not run it. What is shown is camp's own reading of "+
				"git; the step's real output is checked against that assembly "+
				"before anything is mounted")
	}
	return out, refused
}

// Adopt reads what the generation step produced and checks it as hostile
// data, without running anything and without writing anything.
//
// Two processes need this. The launcher calls it as part of Prepare. The
// process that actually mounts -- the session's init, inside the
// namespace -- calls it on its own, so that what it mounts has been
// checked against the repositories by the process doing the mounting, and
// not merely handed to it.
//
// The exclude is always compared against camp's own assembly of it,
// whichever step produced it: the specification requires a custom
// generator's payload to be byte-identical to that assembly, so there is
// exactly one right answer and camp can compute it.
func Adopt(built plan.Plan) (Output, refusal.List) {
	step, has := built.Config.GenerationStep()
	if !has {
		return withoutAStep(built)
	}

	want, refused := expected(built)
	if !refused.Empty() {
		return Output{}, refused
	}

	out := want
	if step.Kind == config.Generate {
		produced, problems := ReadOutputs(built)
		refused.Extend(problems)
		if !refused.Empty() {
			return Output{}, refused
		}
		out = produced
		out.Patterns = want.Patterns
		out.ExcludeFile = want.ExcludeFile
		// Collected rather than returned on: the syntax checks below run
		// too, so one run reports everything wrong with the output instead
		// of one thing per round.
		refused.Extend(matchIslands(built, want, out))
	}

	refused.Extend(Validate(built, out, want.Exclude))
	return out, refused
}

// matchIslands compares a generator's islands with what camp derived
// itself, target by target, exactly.
//
// The syntax checks alone are not enough, and the two ways past them are
// opposite. An entry left out turns a path the source contributes into
// water: it stops being a read-only island and becomes writable
// machine-local storage, so an edit that should have failed loudly
// succeeds and exists in no repository -- the design's worst failure
// shape. An entry added is a name that exists but that the source does
// not contribute -- the source's own runtime junk, or a file somebody
// dropped there -- and mounting it is the generator steering a mount that
// root makes in the privileged mode.
//
// camp has the right answer in its hand either way: it derives the same
// set independently, from what the repository tracks, which is what
// "contributes" means. So the comparison is exact rather than a set of
// rules about what a generator may add.
func matchIslands(built plan.Plan, want, got Output) refusal.List {
	var refused refusal.List
	for _, mount := range built.IslandsMounts {
		target := mount.Target.String()
		expected := byName(want.Islands[target])
		produced := byName(got.Islands[target])

		var missing, extra []string
		for name, kind := range expected {
			if produced[name] != kind {
				missing = append(missing, fmt.Sprintf("%s (%s)", name, kind))
			}
		}
		for name, kind := range produced {
			if expected[name] != kind {
				extra = append(extra, fmt.Sprintf("%s (%s)", name, kind))
			}
		}
		enc.SortNames(missing)
		enc.SortNames(extra)

		if len(missing) > 0 {
			refused.Add("generate-islands-missing",
				"the generation step left %d of the islands at %q out: %s.\n"+
					"An entry the source contributes and the step does not name "+
					"stops being a read-only island and becomes water -- writable, "+
					"machine-local storage -- so an edit that should fail loudly "+
					"would succeed and land in no repository. camp derives the same "+
					"set itself, from what the source tracks, and the two have to "+
					"agree.\ncamp's own list: %s",
				len(missing), target, strings.Join(missing, ", "),
				strings.Join(names(expected), ", "))
		}
		if len(extra) > 0 {
			refused.Add("generate-islands-extra",
				"the generation step named %d islands at %q that the source does "+
					"not contribute: %s.\n"+
					"An island stands for content the source repository tracks. A "+
					"name that merely exists there -- the source's own runtime file, "+
					"something somebody dropped in -- would be mounted on camp's "+
					"say-so, by root in the privileged mode.\ncamp's own list: %s",
				len(extra), target, strings.Join(extra, ", "),
				strings.Join(names(expected), ", "))
		}
	}
	return refused
}

func byName(entries []islands.Entry) map[string]pathx.Type {
	out := make(map[string]pathx.Type, len(entries))
	for _, entry := range entries {
		out[entry.Name] = entry.Type
	}
	return out
}

func names(entries map[string]pathx.Type) []string {
	out := make([]string, 0, len(entries))
	for name, kind := range entries {
		out = append(out, fmt.Sprintf("%s (%s)", name, kind))
	}
	enc.SortNames(out)
	if len(out) == 0 {
		return []string{"(nothing)"}
	}
	return out
}

// withoutAStep is the shape of a composition that generates nothing.
//
// It has no exclude at all -- plan says so plainly rather than leaving
// the defence out quietly -- and its islands, if it has any, come from
// the raw listing of the source, which is said out loud because the
// difference matters: the source's own runtime files become islands.
func withoutAStep(built plan.Plan) (Output, refusal.List) {
	var refused refusal.List
	var out Output
	if len(built.IslandsMounts) == 0 {
		return out, refused
	}

	out.Islands = map[string][]islands.Entry{}
	for _, mount := range built.IslandsMounts {
		// The raw listing, and git is not asked at all. This branch used to
		// call the same function the shipped step calls, which opens git and
		// prefers tracked content when the source happens to be a
		// repository -- so a composition that declares no generation step
		// still had git knowledge in it, and the fallback it documents was
		// not the fallback it ran.
		entries, problems := listed(built, mount)
		refused.Extend(problems)
		out.Islands[mount.Target.String()] = entries
		out.Notes = append(out.Notes,
			"there is no generation step, so the islands at "+mount.Target.String()+
				" come from the raw listing of "+mount.Source+" rather than from "+
				"what it tracks: the source's own runtime files will appear as "+
				"islands")
	}
	return out, refused
}

// listed derives the islands from the source's raw directory listing.
//
// Every entry, tracked or not: without a generation step there is nothing
// that knows what a repository contributes, and camp's core carries no
// git of its own. The note beside it says so, because a raw listing is a
// usable answer and not an equivalent one -- the source's own runtime
// files become islands under it.
func listed(built plan.Plan, mount plan.Islands) ([]islands.Entry, refusal.List) {
	var refused refusal.List
	infos, err := pathx.ReadDirBeneath(built.Config.Env, mount.SourceParts)
	if err != nil {
		refused.Add("generate-listing", "listing %s failed: %v.", mount.Source, err)
		return nil, refused
	}
	return toEntries(infos), refused
}

// expandedChecks re-runs the rules that could not be checked before the
// islands were known.
func expandedChecks(built plan.Plan, out Output) refusal.List {
	var refused refusal.List
	expanded := Expand(built, out)

	repo, state, err := gitwire.Open(built.Config.UpperPath())
	if state == gitwire.Unreadable {
		refused.Add("git-unreadable",
			"git could not say whether %s is a working tree: %v.\n"+
				"The expanded mounts are checked against what the code repository "+
				"tracks, because no mount may cover tracked content. Without an "+
				"answer that check cannot run.", built.Config.UpperPath(), err)
		return refused
	}

	var tracks func(string) []string
	if state == gitwire.InWorkTree {
		tracks = func(path string) []string {
			tracked, err := repo.TracksUnder(path)
			if err != nil {
				// Carried out rather than swallowed: the caller is holding the
				// list this refusal belongs to.
				refused.Add("git-unreadable",
					"git could not say what %s tracks under %q: %v.",
					built.Config.UpperPath(), path, err)
				return nil
			}
			return tracked
		}
	}
	refused.Extend(ValidateExpanded(expanded, tracks))
	return refused
}

// scaffold creates the attachment points the islands need, after their
// list has been validated and never before.
func scaffold(built plan.Plan, out Output, notes *[]string) refusal.List {
	var refused refusal.List
	if len(built.IslandsMounts) == 0 {
		return refused
	}

	storage := fsx.Storage(built.Config.Root, built.Hash)
	manifest, err := islands.LoadManifest(storage)
	if err != nil {
		refused.Add("islands-manifest",
			"%v\nThe manifest is how camp tells its own attachment points from "+
				"your machine-local files. Without it camp cannot prove which is "+
				"which, and it will not guess.", err)
		return refused
	}

	for _, mount := range built.IslandsMounts {
		expansion := islands.Expansion{
			Target:      mount.Target,
			Store:       mount.Store,
			Source:      mount.Source,
			SourceParts: mount.SourceParts,
			Entries:     out.Islands[mount.Target.String()],
		}
		problems, adopted := islands.Scaffold(storage, manifest, expansion)
		refused.Extend(problems)
		*notes = append(*notes, adopted...)
	}
	return refused
}
