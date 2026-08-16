// Package plan turns a configuration into the ordered list of mounts a
// run would make, and changes nothing while doing it.
//
// The run has two halves and the split is the point. The **frame** always
// executes, in a fixed order, and carries everything the safety rests on,
// so no configuration can move it, weaken it or leave it out. The
// configuration's **steps** sit in the middle of the frame and carry what
// is genuinely this composition's decision.
//
// Frame, in order:
//
//  1. the workspace bound onto itself read-only, first, so that while the
//     composition is up the lower cannot be written through its own path
//     either;
//  2. the overlay at the merged root -- workspace below, code on top;
//  3. a read-only bind over every workspace root entry that no mount
//     target covers and allow_overlap does not name. This is what makes
//     a write to a workspace-provided path fail loudly instead of copying
//     up into the code repository, and it is why nothing lower-provided
//     is writable in the steady state.
//
// Then the steps, in their declared order. Then verification, and only
// then is the composition declared up.
//
// Per-file mounting was considered for the third item and rejected: a
// bind is a live view, so protecting the directory already covers files
// born in the workspace mid-session, while per-file coverage would cost
// thousands of mounts and would put a writable store under content
// directories -- where a stray write would be absorbed silently instead
// of refused loudly. "Looks applied, exists nowhere" is the failure this
// design exists to prevent.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/pathx"
)

// Mode is how the composition will be built. It changes exactly one thing
// in the plan -- the overlay's xattr namespace -- and a great deal about
// what is verified afterwards.
type Mode string

const (
	// Namespace is the primary mode: mounts inside a user namespace,
	// invisible to the rest of the machine, torn down by the kernel.
	Namespace Mode = "namespace"
	// Privileged is the fallback: one mount table, visible to every
	// process, for sessions where a program started outside must see the
	// tree.
	Privileged Mode = "privileged"
)

// Xattr returns the extended-attribute namespace the overlay must be told
// to use.
//
// A mount made inside a user namespace cannot write trusted.* at all, so
// it has to use user.*; a privileged mount uses trusted.*, which only
// root can write. The kernel forces userxattr inside a user namespace
// whether or not it was asked for -- camp passes it explicitly anyway and
// verifies per option, because a plan that does not say what it means
// cannot be checked against what happened.
func (m Mode) Xattr() string {
	if m == Namespace {
		return "userxattr"
	}
	return "nouserxattr"
}

// Kind is what one mount does.
type Kind string

const (
	// Overlay is the composed tree itself.
	Overlay Kind = "overlay"
	// BindRO is a read-only bind: made by a bind followed by a read-only
	// remount, never in one call, because MS_BIND|MS_RDONLY in a single
	// mount(2) silently ignores the read-only flag.
	BindRO Kind = "bind-ro"
	// BindRW is a writable bind.
	BindRW Kind = "bind-rw"
)

// Role says why a mount is in the plan. It is what plan prints, and what
// a refusal names when the mount it is talking about was derived rather
// than written down.
type Role string

const (
	// FreezeLower is the workspace's self-bind.
	FreezeLower Role = "freeze-lower"
	// Composed is the overlay.
	Composed Role = "composed"
	// RootGuard is a derived read-only bind over a workspace root entry.
	RootGuard Role = "root-guard"
	// Declared is a mount the configuration asked for by name.
	Declared Role = "declared"
	// Store is the writable machine-local floor of an islands mount, or a
	// sourceless mount_rw's storage.
	Store Role = "store"
	// Island is one read-only entry standing in that floor.
	Island Role = "island"
	// Artefact is the generated exclude, bound over the live tree's copy.
	Artefact Role = "artefact"
)

// Mount is one operation, with the sentence explaining why it exists.
type Mount struct {
	Kind Kind
	Role Role

	// Source is absolute. For the overlay it is empty.
	Source string
	// Target is absolute.
	Target string

	// SourceParts and TargetParts are the same two paths written as
	// components beneath the environment root.
	//
	// They are what every resolution actually uses: opened one component
	// at a time, descriptor-relative, following no symlink and never
	// leaving the root. The absolute strings above are for messages. A
	// check made on a string and a mount made on a path are two different
	// objects whenever any component of the path is a link, and that gap
	// is exactly where a swapped component would slip through.
	SourceParts []string
	TargetParts []string
	// Rel is the target relative to the merged root. It is the zero value
	// for the one mount that lives outside the tree -- the workspace's
	// self-bind.
	Rel pathx.Rel
	// InLive is false only for that same mount.
	InLive bool

	// Type is what both ends have to be: a directory bind onto a
	// directory, a file bind onto a file.
	Type pathx.Type

	// Step is the index of the configuration step this came from, or -1
	// for the frame.
	Step int

	// Why is the reason, in one sentence, for whoever reads the plan.
	Why string

	// Overlay only.
	Lower []string
	Upper string
	Work  string
	Xattr string
}

// Describe renders one mount for a person reading the plan.
//
// Labels, never lowerdir's positional syntax: left-to-right precedence is
// exactly what people read backwards.
func (m Mount) Describe() string {
	switch m.Kind {
	case Overlay:
		return fmt.Sprintf("overlay  %s\n           lower (read-only): %s\n"+
			"           upper (writes land here): %s\n           workdir: %s\n"+
			"           xattr: %s",
			m.Target, strings.Join(m.Lower, ", "), m.Upper, m.Work, m.Xattr)
	case BindRO:
		return fmt.Sprintf("bind,ro  %s  <-  %s", m.Target, m.Source)
	default:
		return fmt.Sprintf("bind,rw  %s  <-  %s", m.Target, m.Source)
	}
}

// Islands is one mount_islands entry, before its entries are known.
//
// The store is planned statically; what stands in it comes from the
// generation pass, because "what the source contributes" is a question
// only a generator can answer -- git by default.
type Islands struct {
	// Step is the configuration step it came from.
	Step int
	// Source is the absolute path of the directory whose contributed
	// entries become islands.
	Source string
	// SourceParts is the same path as components beneath the environment
	// root, for resolution that follows no symlink.
	SourceParts []string
	// Target is the mount point, relative to the merged root.
	Target pathx.Rel
	// Store is the absolute path of the writable floor.
	Store string
}

// Plan is everything an up would do.
type Plan struct {
	Config config.Config
	Mode   Mode

	// Live is the merged root, absolute.
	Live string
	// Hash identifies this composition by its live path: the first twelve
	// hex characters of SHA-256 over the live directory's realpath. Never
	// random, so an orphan left by a crash can be attributed, and stable,
	// because the work and storage directories are named from it.
	Hash string
	// Work is disposable: the overlay workdir, the generated exclude, the
	// staging tree. Garbage-collectable whenever nothing is mounted.
	Work string
	// Storage is persistent: island stores, writable holes, worktrees.
	// Never removed by camp -- it holds unfinished work.
	Storage string
	// OverlayWork is the directory the kernel is given as workdir.
	OverlayWork string

	// Mounts is the whole sequence, frame and steps interleaved in the
	// order they happen.
	Mounts []Mount
	// IslandsMounts are the entries still to be expanded.
	IslandsMounts []Islands

	// LowerRoot and UpperRoot are the raw root listings the gate, the
	// derived protections and the exclude all read. One enumeration, so
	// they cannot describe different sets.
	LowerRoot []pathx.Info
	UpperRoot []pathx.Info
}

// Hash derives a composition's identifier from its live path.
func Hash(realLive string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(realLive, "/")))
	return hex.EncodeToString(sum[:])[:12]
}

// WorkDir and StorageDir are the two stores, which never share a parent
// or a naming scheme because their lifecycles are opposite: work may be
// swept whenever nothing is mounted, storage may never be removed at all.
func WorkDir(env, hash string) string {
	return filepath.Join(env, config.Dir, "work", hash)
}

// StorageDir is the persistent store for a composition.
func StorageDir(env, hash string) string {
	return filepath.Join(env, config.Dir, "storage", hash)
}

// ReportsDir is where a namespace session leaves its end-of-session
// report: output to be read once, never authority.
func ReportsDir(env string) string {
	return filepath.Join(env, config.Dir, "reports")
}

// storePath mirrors a target's path inside the storage directory rather
// than escaping it into one component.
//
// Escaping the separator would blow past the 255-byte limit on a single
// name for any deep target, so the path is reproduced as a path.
func storePath(storage string, target pathx.Rel) string {
	return target.Join(storage)
}

// ExcludeTarget is where a generated exclude is mounted: the live tree's
// copy, never the repository's.
//
// Measured: a bind there is visible only through the composed tree, so
// the code repository and any checkout registered outside keep reading
// their own file. Under the privileged mode that scoping is the whole
// difference between a composition detail and a machine-wide change to
// what git reports in the code repository.
var ExcludeTarget = []string{".git", "info", "exclude"}

// ExcludeFile is the generated payload's path inside the work directory.
func (p Plan) ExcludeFile() string { return filepath.Join(p.Work, "exclude") }

// GenDir is the generation step's scratch: its inputs, its outputs, and
// its working directory, so a naive generator's relative writes land in
// camp's scratch and never in a repository.
func (p Plan) GenDir() string { return filepath.Join(p.Work, "gen") }

// StagingRoot is where the privileged mode builds the whole composition
// before moving it onto the live directory.
func (p Plan) StagingRoot() string { return filepath.Join(p.Work, "staging") }

// Targets returns every mount point in the order they are created.
func (p Plan) Targets() []string {
	targets := make([]string, 0, len(p.Mounts))
	for _, mount := range p.Mounts {
		targets = append(targets, mount.Target)
	}
	return targets
}

// Teardown returns the mounts in the order they come down: the reverse of
// the order they went up.
func (p Plan) Teardown() []Mount {
	reversed := make([]Mount, 0, len(p.Mounts))
	for i := len(p.Mounts) - 1; i >= 0; i-- {
		reversed = append(reversed, p.Mounts[i])
	}
	return reversed
}

// rootTargets returns the step targets that are a single component --
// the only ones that can completely cover a workspace root entry.
func rootTargets(cfg config.Config) map[string]bool {
	covered := map[string]bool{}
	for _, step := range cfg.Steps {
		for _, entry := range step.Entries {
			if len(entry.Target.Components()) == 1 {
				covered[entry.Target.First()] = true
			}
		}
	}
	return covered
}

// Build derives the plan from a configuration and the two root listings.
//
// It reads directory listings and nothing else, and it changes nothing.
// That is what makes plan honest rather than approximate, and what lets
// the whole derivation be tested without root.
func Build(cfg config.Config, mode Mode, live, hash string, lowerRoot, upperRoot []pathx.Info) Plan {
	work := WorkDir(cfg.Env, hash)
	storage := StorageDir(cfg.Env, hash)
	lowerPath := cfg.LowerPath()
	upperPath := cfg.UpperPath()

	p := Plan{
		Config:      cfg,
		Mode:        mode,
		Live:        live,
		Hash:        hash,
		Work:        work,
		Storage:     storage,
		OverlayWork: filepath.Join(work, "work"),
		LowerRoot:   lowerRoot,
		UpperRoot:   upperRoot,
	}

	lowerParts := repositoryParts(cfg, cfg.Lower[0])
	liveParts := cfg.Merged.Components()
	storeParts := []string{config.Dir, "storage", hash}

	// 1. The workspace, frozen against its own path.
	p.Mounts = append(p.Mounts, Mount{
		Kind:        BindRO,
		Role:        FreezeLower,
		Source:      lowerPath,
		Target:      lowerPath,
		SourceParts: lowerParts,
		TargetParts: lowerParts,
		Type:        pathx.Dir,
		Step:        -1,
		Why: fmt.Sprintf("hold %s read-only while the composition is up, so a "+
			"process inside cannot write the workspace even by its absolute path",
			lowerPath),
	})

	// 2. The composed tree.
	p.Mounts = append(p.Mounts, Mount{
		Kind:        Overlay,
		Role:        Composed,
		Target:      live,
		TargetParts: liveParts,
		Rel:         pathx.Rel{},
		InLive:      true,
		Type:        pathx.Dir,
		Step:        -1,
		Lower:       []string{lowerPath},
		Upper:       upperPath,
		Work:        p.OverlayWork,
		Xattr:       mode.Xattr(),
		Why: "the composed tree: the workspace read-only underneath, the code " +
			"repository on top, where every ordinary write lands",
	})

	// 3. The derived root protections.
	covered := rootTargets(cfg)
	for _, entry := range lowerRoot {
		if covered[entry.Name] || cfg.AllowsOverlap(entry.Name) {
			continue
		}
		rel := pathx.Rel{}.Append(entry.Name)
		p.Mounts = append(p.Mounts, Mount{
			Kind:        BindRO,
			Role:        RootGuard,
			Source:      filepath.Join(lowerPath, entry.Name),
			Target:      filepath.Join(live, entry.Name),
			SourceParts: join(lowerParts, entry.Name),
			TargetParts: join(liveParts, entry.Name),
			Rel:         rel,
			InLive:      true,
			Type:        entry.Type,
			Step:        -1,
			Why: fmt.Sprintf("%q comes from the workspace; bound back read-only so "+
				"that writing it through the composed tree fails loudly instead of "+
				"copying it up into the code repository", entry.Name),
		})
	}

	// 4. The configuration's own steps, in their declared order.
	for index, step := range cfg.Steps {
		switch step.Kind {
		case config.MountRO, config.MountRW:
			for _, entry := range step.Entries {
				p.Mounts = append(p.Mounts, declaredMount(cfg, p, index, step, entry, live,
					liveParts, storeParts))
			}
		case config.MountIslands:
			for _, entry := range step.Entries {
				store := storePath(storage, entry.Target)
				p.Mounts = append(p.Mounts, Mount{
					Kind:        BindRW,
					Role:        Store,
					Source:      store,
					Target:      entry.Target.Join(live),
					SourceParts: join(storeParts, entry.Target.Components()...),
					TargetParts: join(liveParts, entry.Target.Components()...),
					Rel:         entry.Target,
					InLive:      true,
					Type:        pathx.Dir,
					Step:        index,
					Why: fmt.Sprintf("the writable floor of the islands mount at %q: "+
						"machine-local storage, so runtime files that exist in no "+
						"repository have somewhere to live and survive the session",
						entry.Target.String()),
				})
				p.IslandsMounts = append(p.IslandsMounts, Islands{
					Step:        index,
					Source:      sourcePath(cfg, entry.Source),
					SourceParts: sourceParts(cfg, entry.Source),
					Target:      entry.Target,
					Store:       store,
				})
			}
		case config.GitExclude, config.Generate:
			rel := pathx.Rel{}
			for _, part := range ExcludeTarget {
				rel = rel.Append(part)
			}
			p.Mounts = append(p.Mounts, Mount{
				Kind:        BindRO,
				Role:        Artefact,
				Source:      p.ExcludeFile(),
				Target:      rel.Join(live),
				SourceParts: []string{config.Dir, "work", hash, "exclude"},
				TargetParts: join(liveParts, rel.Components()...),
				Rel:         rel,
				InLive:      true,
				Type:        pathx.File,
				Step:        index,
				Why: "the generated exclude, mounted over the composed tree's copy " +
					"so that git run from here ignores the workspace's names -- the " +
					"repository's own file is untouched and keeps its own content",
			})
		}
	}

	return p
}

func declaredMount(cfg config.Config, p Plan, index int, step config.Step, entry config.Entry, live string, liveParts, storeParts []string) Mount {
	kind := BindRW
	if step.Kind == config.MountRO {
		kind = BindRO
	}

	if entry.Source == nil {
		return Mount{
			Kind:        BindRW,
			Role:        Store,
			Source:      storePath(p.Storage, entry.Target),
			Target:      entry.Target.Join(live),
			SourceParts: join(storeParts, entry.Target.Components()...),
			TargetParts: join(liveParts, entry.Target.Components()...),
			Rel:         entry.Target,
			InLive:      true,
			Type:        pathx.Dir,
			Step:        index,
			Why: fmt.Sprintf("%q is a writable hole: camp provides empty "+
				"machine-local storage for it, and it belongs to no repository",
				entry.Target.String()),
		}
	}

	why := fmt.Sprintf("%q is mounted read-only from %s", entry.Target.String(), entry.Source)
	if kind == BindRW {
		why = fmt.Sprintf("%q accepts writes, and they land in %s -- not in the "+
			"code repository and not in the workspace", entry.Target.String(), entry.Source)
	}
	return Mount{
		Kind:        kind,
		Role:        Declared,
		Source:      sourcePath(cfg, entry.Source),
		Target:      entry.Target.Join(live),
		SourceParts: sourceParts(cfg, entry.Source),
		TargetParts: join(liveParts, entry.Target.Components()...),
		Rel:         entry.Target,
		InLive:      true,
		Step:        index,
		Why:         why,
	}
}

func sourcePath(cfg config.Config, source *config.Source) string {
	if source == nil {
		return ""
	}
	return source.Path.Join(cfg.RepositoryPath(source.Repository))
}

// repositoryParts writes a repository's root as components under env.
func repositoryParts(cfg config.Config, name string) []string {
	repo, ok := cfg.Repository(name)
	if !ok {
		return nil
	}
	return repo.Path.Components()
}

func sourceParts(cfg config.Config, source *config.Source) []string {
	if source == nil {
		return nil
	}
	return join(repositoryParts(cfg, source.Repository), source.Path.Components()...)
}

func join(base []string, more ...string) []string {
	out := make([]string, 0, len(base)+len(more))
	out = append(out, base...)
	return append(out, more...)
}
