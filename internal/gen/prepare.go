package gen

import (
	"path/filepath"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/gitwire"
	"github.com/dlaszlo/camp/internal/islands"
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
	if !has {
		return withoutAStep(built)
	}

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

	refused.Extend(expandedChecks(built, out))
	if !refused.Empty() {
		return out, refused
	}

	refused.Extend(scaffold(built, out, &out.Notes))
	if !refused.Empty() {
		return out, refused
	}

	if err := fsx.Work(built.Work).Write("exclude", out.Exclude, 0o644); err != nil {
		refused.Add("generate-write", "%v", err)
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
	}

	refused.Extend(Validate(built, out, want.Exclude))
	return out, refused
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
		entries, _, problems := contributed(built, mount)
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

// expandedChecks re-runs the rules that could not be checked before the
// islands were known.
func expandedChecks(built plan.Plan, out Output) refusal.List {
	expanded := Expand(built, out)
	var tracks func(string) []string
	if repo, isGit := gitwire.Open(built.Config.UpperPath()); isGit {
		tracks = func(path string) []string {
			tracked, err := repo.TracksUnder(path)
			if err != nil {
				return nil
			}
			return tracked
		}
	}
	return ValidateExpanded(expanded, tracks)
}

// scaffold creates the attachment points the islands need, after their
// list has been validated and never before.
func scaffold(built plan.Plan, out Output, notes *[]string) refusal.List {
	var refused refusal.List
	if len(built.IslandsMounts) == 0 {
		return refused
	}

	storage := fsx.Storage(built.Storage)
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
