package gen

import (
	"path/filepath"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
)

// Output is everything the prepare phase produced.
type Output struct {
	// Exclude is the payload to mount, complete: the repository's own
	// bytes plus camp's block. Empty when no step generates one, and then
	// nothing checks for one either.
	Exclude []byte
	// Patterns are camp's own lines, for plan to print.
	Patterns []string
	// ExcludeFile is where the payload was written.
	ExcludeFile string
}

// Derive computes what the generation step must produce, without writing
// anything.
//
// It is a pure function of the two repositories and the plan, and that is
// what makes it useful twice. The launcher calls Prepare, which derives
// and writes. The process that actually mounts derives again,
// independently, and verification then compares the mounted file against
// *this* payload rather than against the file it just mounted -- which
// would be circular and would prove nothing.
func Derive(built plan.Plan) (Output, refusal.List) {
	var refused refusal.List
	var out Output

	step, has := built.Config.GenerationStep()
	if !has {
		return out, refused
	}
	if step.Kind != config.GitExclude {
		refused.Add("generate-not-supported",
			"the configuration uses a custom generate step, and this build "+
				"runs only the shipped git_exclude step.")
		return out, refused
	}

	existing, problems := ReadExisting(
		filepath.Join(built.Config.UpperPath(), ".git", "info", "exclude"))
	refused.Extend(problems)

	patterns, problems := ExcludeLines(built.Config, built)
	refused.Extend(problems)
	if !refused.Empty() {
		return out, refused
	}

	out.Patterns = patterns
	out.Exclude = ExcludePayload(existing, built.Hash, patterns)
	out.ExcludeFile = built.ExcludeFile()
	return out, refused
}

// Prepare runs the generation step and writes what it produced.
//
// It runs before anything is mounted and always as the invoking user. In
// the privileged mode that is true by construction rather than by a
// drop protocol: prepare happens in the unprivileged front end, and no
// privileged camp process ever executes a configured command. Whoever can
// edit the configuration must never gain root through it.
func Prepare(built plan.Plan) (Output, refusal.List) {
	out, refused := Derive(built)
	if !refused.Empty() || len(out.Exclude) == 0 {
		return out, refused
	}
	if err := fsx.Work(built.Work).Write("exclude", out.Exclude, 0o644); err != nil {
		refused.Add("generate-write", "%v", err)
	}
	return out, refused
}
