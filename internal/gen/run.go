package gen

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/gitwire"
	"github.com/dlaszlo/camp/internal/islands"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
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
		entries, problems := contributed(built, mount)
		refused.Extend(problems)
		out.Islands[mount.Target.String()] = entries
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
func contributed(built plan.Plan, mount plan.Islands) ([]islands.Entry, refusal.List) {
	var refused refusal.List

	repository := built.Config.RepositoryPath(mount.Repository)
	if repo, isGit := gitwire.Open(repository); isGit {
		infos, err := repo.Contributes(mount.Relative)
		if err != nil {
			refused.Add("generate-git",
				"asking git what %s contributes at %q failed: %v.",
				repository, mount.Relative, err)
			return nil, refused
		}
		return toEntries(infos), refused
	}

	infos, err := pathx.ReadDirBeneath(built.Config.Env, mount.SourceParts)
	if err != nil {
		refused.Add("generate-listing",
			"listing %s failed: %v.", mount.Source, err)
		return nil, refused
	}
	return toEntries(infos), refused
}

// IsGitBacked reports whether an islands source is a git repository, so
// that doctor can say when the fallback is in use.
func IsGitBacked(built plan.Plan, mount plan.Islands) bool {
	_, isGit := gitwire.Open(built.Config.RepositoryPath(mount.Repository))
	return isGit
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
// It always runs as the invoking user. In the privileged mode that is
// true by construction rather than by a drop protocol: prepare runs in
// the unprivileged front end, and no privileged camp process ever
// executes a configured command.
func external(built plan.Plan, step config.Step) refusal.List {
	var refused refusal.List
	paths := PathsFor(built)

	if os.Geteuid() == 0 {
		refused.Add("generate-privileged",
			"the configuration's generation step would run as root, and camp "+
				"never runs configured code with privilege.\n"+
				"Whoever can edit the configuration would otherwise gain root "+
				"through it. Run 'camp up' without sudo: the front end does the "+
				"generating as you, and only the mounting itself is elevated.")
		return refused
	}

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		refused.Add("generate-run", "opening %s: %v", os.DevNull, err)
		return refused
	}
	defer devnull.Close()

	command := exec.Command(step.Command[0], step.Command[1:]...)
	command.Dir = paths.Root
	command.Stdin = devnull
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = append(os.Environ(),
		"CAMP_GEN_IN="+paths.In,
		"CAMP_GEN_OUT="+paths.Out,
		"CAMP_ENV="+built.Config.Env,
		"CAMP_LIVE="+built.Live,
	)
	// Its own process group, so that a timeout can end the whole thing
	// rather than a parent that has already forked.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := command.Start(); err != nil {
		refused.Add("generate-run",
			"the generation step could not be started: %v\n  command: %v",
			err, step.Command)
		return refused
	}

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()

	var timeout <-chan time.Time
	if step.Timeout > 0 {
		timer := time.NewTimer(step.Timeout)
		defer timer.Stop()
		timeout = timer.C
	}

	select {
	case err := <-done:
		if err != nil {
			refused.Add("generate-failed",
				"the generation step failed: %v\n  command: %v\n"+
					"Nothing has been mounted: generation happens before any mount, "+
					"so a step that fails stops the composition with the machine "+
					"exactly as it was.", err, step.Command)
		}
	case <-timeout:
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-done
		refused.Add("generate-timeout",
			"the generation step did not finish within %s and its process group "+
				"was killed.\n  command: %v", step.Timeout, step.Command)
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
				TargetParts: append(append([]string{}, built.Config.Merged.Components()...),
					rel.Components()...),
				Rel:    rel,
				InLive: true,
				Type:   entry.Type,
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
