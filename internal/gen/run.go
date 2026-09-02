package gen

import (
	"os"
	"path/filepath"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/gitwire"
	"github.com/dlaszlo/camp/internal/islands"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/runx"
)

// git runs the shipped generator: reads git, produces the exclude payload
// and the islands expansions.
//
// Reads only. There is no function in gitwire that writes, which is the
// shape the first invariant takes here.
func git(built plan.Plan, existing []byte) (Output, refusal.List) {
	var refused refusal.List
	var out Output

	// The shipped step is defined as reading git. Without git it would
	// quietly become something else -- islands from raw directory
	// listings, carrying the source's own runtime files -- so it refuses
	// instead.
	if err := gitwire.Available(); err != nil {
		refused.Add("generate-git-missing",
			"the configuration uses the git_exclude step, and %v.\n"+
				"That step reads git to work out what each repository "+
				"contributes; without it the islands would silently come from raw "+
				"directory listings and carry files no repository tracks. Install "+
				"git, or drop the step -- a composition with no generation step is "+
				"legal, and 'camp plan' says what it costs.", err)
		return out, refused
	}

	patterns, problems := ExcludeLines(built.Config, built)
	refused.Extend(problems)
	out.Patterns = patterns
	out.Exclude = ExcludePayload(existing, built.Hash, patterns)

	out.Islands = map[string][]islands.Entry{}
	for _, mount := range built.IslandsMounts {
		entries, fromGit, problems := contributed(built, mount)
		refused.Extend(problems)
		out.Islands[mount.Target.String()] = entries
		if !fromGit {
			out.Notes = append(out.Notes,
				"the islands at "+mount.Target.String()+" come from the raw "+
					"listing of "+mount.Source+", because that source is not in a "+
					"git repository: every entry the directory happens to hold "+
					"becomes an island, the source's own runtime files included")
		}
	}
	return out, refused
}

// contributed asks what a repository contributes at a path.
//
// Derived from tracked content, not from the raw listing: the raw listing
// would hand out islands to the source's own runtime junk -- its
// settings.local.json, its lock files -- which is precisely what the
// islands mount exists to keep out of the composed tree. A source that is
// not a git repository falls back to the raw listing, and doctor says so
// rather than letting the difference go unnoticed.
// It reports which of the two answered, because the difference has to be
// said out loud where it is noticed rather than absorbed: a raw listing is
// a usable answer, not an equivalent one.
func contributed(built plan.Plan, mount plan.Islands) ([]islands.Entry, bool, refusal.List) {
	var refused refusal.List

	repository := built.Config.RepositoryPath(mount.Repository)
	repo, state, err := gitwire.Open(repository)
	if state == gitwire.Unreadable {
		// Never the raw listing in this case. The listing is a usable answer
		// when the source is not a repository at all; here git exists and
		// could not answer, and falling back would hand out islands to the
		// source's own runtime junk -- which is what the islands mount is
		// for keeping out -- while looking like an ordinary composition.
		refused.Add("generate-git",
			"git could not say whether %s is a working tree: %v.\n"+
				"The islands are derived from what the repository tracks there. A "+
				"raw listing would include the source's own untracked files, which "+
				"is exactly what this mount exists to keep out of the composed "+
				"tree, so camp stops instead.", repository, err)
		return nil, true, refused
	}
	if state == gitwire.InWorkTree {
		infos, err := repo.Contributes(mount.Relative)
		if err != nil {
			refused.Add("generate-git",
				"asking git what %s contributes at %q failed: %v.",
				repository, mount.Relative, err)
			return nil, true, refused
		}
		return toEntries(infos), true, refused
	}

	infos, err := pathx.ReadDirBeneath(built.Config.Env, mount.SourceParts)
	if err != nil {
		refused.Add("generate-listing",
			"listing %s failed: %v.", mount.Source, err)
		return nil, false, refused
	}
	return toEntries(infos), false, refused
}

func toEntries(infos []pathx.Info) []islands.Entry {
	entries := make([]islands.Entry, 0, len(infos))
	for _, info := range infos {
		entries = append(entries, islands.Entry{Name: info.Name, Type: info.Type})
	}
	return entries
}

// external runs a configured generator.
//
// The contract, and the reasons for each part of it. `command` is an argv
// vector executed directly -- never through a shell, so nothing is split
// on spaces and nothing is expanded between the file and the process. The
// working directory is camp's own scratch, so a naive generator's
// relative writes land there and not in a repository. stdin is /dev/null;
// stdout and stderr go straight to the terminal, because a generator that
// wants to say something should be able to. There is no default timeout,
// because camp is driven from a terminal by somebody who can interrupt
// it, and an optional one kills the process group when it expires.
//
// It always runs as the invoking user: camp holds no privilege at this
// point and never acquires any in the process that runs configured code.
//
// Starting it and waiting for it is runx's -- the same mechanism the
// prepare commands run on. The wording is here, because a generation step
// that failed and a guard that refused are two different pieces of news.
func external(built plan.Plan, step config.Step) refusal.List {
	var refused refusal.List
	paths := PathsFor(built)

	if os.Geteuid() == 0 {
		refused.Add("generate-privileged",
			"the configuration's generation step would run as root, and camp "+
				"never runs configured code with privilege.\n"+
				"Whoever can edit the configuration would otherwise gain root "+
				"through it. Run the same command again as yourself, without "+
				"sudo: camp needs no privilege, and mounts inside a namespace of "+
				"its own.")
		return refused
	}

	result := runx.Run(runx.Command{
		Argv: step.Command,
		Dir:  paths.Root,
		Env: append(os.Environ(),
			"CAMP_GEN_IN="+paths.In,
			"CAMP_GEN_OUT="+paths.Out,
			"CAMP_ENV="+built.Config.Env,
			"CAMP_LIVE="+built.Live),
		Timeout: step.Timeout,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	})

	switch result.Outcome {
	case runx.OK:
	case runx.NotStarted:
		refused.Add("generate-run",
			"the generation step could not be started: %v\n  command: %v",
			result.Err, step.Command)
	case runx.Failed:
		refused.Add("generate-failed",
			"the generation step failed: %v\n  command: %v\n"+
				"Nothing has been mounted: generation happens before any mount, "+
				"so a step that fails stops the composition with the machine "+
				"exactly as it was.", result.Err, step.Command)
	case runx.TimedOut:
		refused.Add("generate-timeout",
			"the generation step did not finish within %s and its process group "+
				"was killed.\n  command: %v", step.Timeout, step.Command)
	case runx.Interrupted:
		refused.Add("generate-interrupted",
			"the generation step was interrupted (%s) and its process group "+
				"was sent the same signal.\n  command: %v\n"+
				"Nothing has been mounted: generation happens before any mount.",
			result.Signal, step.Command)
	}
	return refused
}

// Expand folds the islands into the plan, each entry's mounts placed
// immediately after its store.
//
// The order inside one islands entry -- the store first, then its islands
// -- is internal and cannot be misdeclared. Where the entry sits among
// the other mounts is the steps order, which the configuration decides.
func Expand(built plan.Plan, out Output) plan.Plan {
	expanded := built
	expanded.Mounts = nil

	byTarget := map[string]plan.Islands{}
	for _, mount := range built.IslandsMounts {
		byTarget[mount.Target.String()] = mount
	}

	for _, mount := range built.Mounts {
		expanded.Mounts = append(expanded.Mounts, mount)
		if mount.Role != plan.Store {
			continue
		}
		islandsMount, isIslands := byTarget[mount.Rel.String()]
		if !isIslands {
			continue
		}
		for _, entry := range out.Islands[mount.Rel.String()] {
			rel := mount.Rel.Append(entry.Name)
			expanded.Mounts = append(expanded.Mounts, plan.Mount{
				Kind:   plan.BindRO,
				Role:   plan.Island,
				Source: filepath.Join(islandsMount.Source, entry.Name),
				Target: rel.Join(built.Live),
				SourceParts: append(append([]string{}, islandsMount.SourceParts...),
					entry.Name),
				Rel:    rel,
				InLive: true,
				Step:   islandsMount.Step,
				Why: "an island: " + entry.Name + " is contributed by " +
					islandsMount.Source + " and stands read-only in the writable " +
					"floor, so editing it through the composed tree fails loudly " +
					"instead of copying anywhere",
			})
		}
	}
	return expanded
}
