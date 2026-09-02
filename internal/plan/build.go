package plan

import (
	"fmt"
	"path/filepath"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/pathx"
)

// Build derives the plan from a configuration and the two root listings.
//
// It reads directory listings and nothing else, and it changes nothing.
// That is what makes plan honest rather than approximate, and what lets
// the whole derivation be tested without root.
//
// The order is the frame's, and the frame is not negotiable: the
// workspace is frozen first, then the composed tree, then the derived
// protections, and only then whatever the configuration asked for.
func Build(cfg config.Config, live, hash string, lowerRoot, upperRoot []pathx.Info) Plan {
	b := newBuilder(cfg, live, hash, lowerRoot, upperRoot)
	b.freezeLower()
	b.composedTree()
	b.rootGuards()
	b.steps()
	return b.plan
}

// builder carries what every mount needs to name itself, so that the
// four derivation steps below read as what they are rather than as
// argument lists.
type builder struct {
	plan Plan

	cfg  config.Config
	live string
	hash string

	lowerPath string
	upperPath string

	// The same three paths as components beneath the environment root,
	// which is how every mount is really resolved.
	lowerParts []string
	liveParts  []string
	storeParts []string
}

func newBuilder(cfg config.Config, live, hash string, lowerRoot, upperRoot []pathx.Info) *builder {
	work := WorkDir(cfg.Env, hash)
	return &builder{
		plan: Plan{
			Config:      cfg,
			Live:        live,
			Hash:        hash,
			Work:        work,
			Storage:     StorageDir(cfg.Env, hash),
			OverlayWork: filepath.Join(work, "work"),
			LowerRoot:   lowerRoot,
			UpperRoot:   upperRoot,
		},
		cfg:        cfg,
		live:       live,
		hash:       hash,
		lowerPath:  cfg.LowerPath(),
		upperPath:  cfg.UpperPath(),
		lowerParts: repositoryParts(cfg, cfg.Lower[0]),
		liveParts:  cfg.Merged.Components(),
		storeParts: []string{config.Dir, "storage", hash},
	}
}

func (b *builder) add(mount Mount) { b.plan.Mounts = append(b.plan.Mounts, mount) }

// freezeLower holds the workspace read-only on its own path, first.
//
// While the composition is up, a process inside cannot write the
// workspace even by absolute path. It comes first so that there is no
// window in which the lower is both visible and writable.
func (b *builder) freezeLower() {
	b.add(Mount{
		Kind:        BindRO,
		Role:        FreezeLower,
		Source:      b.lowerPath,
		Target:      b.lowerPath,
		SourceParts: b.lowerParts,
		TargetParts: b.lowerParts,
		Type:        pathx.Dir,
		Step:        -1,
		Why: fmt.Sprintf("hold %s read-only while the composition is up, so a "+
			"process inside cannot write the workspace even by its absolute path",
			b.lowerPath),
	})
}

// composedTree is the overlay: the workspace underneath, the code
// repository on top and writable.
func (b *builder) composedTree() {
	b.add(Mount{
		Kind:        Overlay,
		Role:        Composed,
		Target:      b.live,
		TargetParts: b.liveParts,
		InLive:      true,
		Type:        pathx.Dir,
		Step:        -1,
		Lower:       []string{b.lowerPath},
		Upper:       b.upperPath,
		Work:        b.plan.OverlayWork,
		Xattr:       UserXattr,
		Why: "the composed tree: the workspace read-only underneath, the code " +
			"repository on top, where every ordinary write lands",
	})
}

// rootGuards binds every workspace root entry back over the composed
// tree, read-only.
//
// Derived from the raw listing, with no name in the configuration. This
// is what makes a write to a workspace-provided path fail loudly instead
// of copying it up into the code repository -- and it is why, in the
// steady state, nothing the workspace provides is writable at all.
//
// A name a mount target already covers is skipped, and so is one
// allow_overlap names: the first is not visible to protect, and the
// second would hide the code repository's own copy.
func (b *builder) rootGuards() {
	covered := rootTargets(b.cfg)
	for _, entry := range b.plan.LowerRoot {
		if covered[entry.Name] || b.cfg.AllowsOverlap(entry.Name) {
			continue
		}
		b.add(Mount{
			Kind:        BindRO,
			Role:        RootGuard,
			Source:      filepath.Join(b.lowerPath, entry.Name),
			Target:      filepath.Join(b.live, entry.Name),
			SourceParts: join(b.lowerParts, entry.Name),
			TargetParts: join(b.liveParts, entry.Name),
			Rel:         pathx.Rel{}.Append(entry.Name),
			InLive:      true,
			Type:        entry.Type,
			Step:        -1,
			Why: fmt.Sprintf("%q comes from the workspace; bound back read-only so "+
				"that writing it through the composed tree fails loudly instead of "+
				"copying it up into the code repository", entry.Name),
		})
	}
}

// steps adds what the configuration asked for, in its own order.
func (b *builder) steps() {
	for index, step := range b.cfg.Steps {
		for _, entry := range step.Entries {
			switch step.Kind {
			case config.MountRO, config.MountRW:
				b.add(b.declared(index, step, entry))
			case config.MountIslands:
				b.islands(index, entry)
			}
		}
		if step.Kind.Generates() {
			b.artefact(index)
		}
	}
}

// declared is one mount the configuration named.
func (b *builder) declared(index int, step config.Step, entry config.Entry) Mount {
	if entry.Source == nil {
		return b.store(index, entry.Target, fmt.Sprintf(
			"%q is a writable hole: camp provides empty machine-local storage "+
				"for it, and it belongs to no repository", entry.Target.String()))
	}

	kind := BindRW
	why := fmt.Sprintf("%q accepts writes, and they land in %s -- not in the "+
		"code repository and not in the workspace", entry.Target.String(), entry.Source)
	if step.Kind == config.MountRO {
		kind = BindRO
		why = fmt.Sprintf("%q is mounted read-only from %s",
			entry.Target.String(), entry.Source)
	}

	return Mount{
		Kind:        kind,
		Role:        Declared,
		Source:      sourcePath(b.cfg, entry.Source),
		Target:      entry.Target.Join(b.live),
		SourceParts: sourceParts(b.cfg, entry.Source),
		TargetParts: join(b.liveParts, entry.Target.Components()...),
		Rel:         entry.Target,
		InLive:      true,
		Step:        index,
		Why:         why,
	}
}

// islands adds the writable floor, and records what still has to be
// expanded onto it once the generation step has said what the source
// contributes.
func (b *builder) islands(index int, entry config.Entry) {
	b.add(b.store(index, entry.Target, fmt.Sprintf(
		"the writable floor of the islands mount at %q: machine-local "+
			"storage, so runtime files that exist in no repository have "+
			"somewhere to live and survive the session", entry.Target.String())))

	islands := Islands{
		Step:        index,
		Source:      sourcePath(b.cfg, entry.Source),
		SourceParts: sourceParts(b.cfg, entry.Source),
		Target:      entry.Target,
		Store:       storePath(b.plan.Storage, entry.Target),
	}
	if entry.Source != nil {
		islands.Repository = entry.Source.Repository
		islands.Relative = entry.Source.Path.String()
	}
	b.plan.IslandsMounts = append(b.plan.IslandsMounts, islands)
}

// store is camp's own writable storage, mounted over a target: the floor
// of an islands mount, or a sourceless writable hole. The two differ
// only in what stands in them afterwards.
func (b *builder) store(index int, target pathx.Rel, why string) Mount {
	return Mount{
		Kind:        BindRW,
		Role:        Store,
		Source:      storePath(b.plan.Storage, target),
		Target:      target.Join(b.live),
		SourceParts: join(b.storeParts, target.Components()...),
		TargetParts: join(b.liveParts, target.Components()...),
		Rel:         target,
		InLive:      true,
		Type:        pathx.Dir,
		Step:        index,
		Why:         why,
	}
}

// artefact is the generated exclude, bound over the composed tree's copy.
func (b *builder) artefact(index int) {
	rel := pathx.Rel{}
	for _, part := range ExcludeTarget {
		rel = rel.Append(part)
	}
	b.add(Mount{
		Kind:        BindRO,
		Role:        Artefact,
		Source:      b.plan.ExcludeFile(),
		Target:      rel.Join(b.live),
		SourceParts: []string{config.Dir, "work", b.hash, "exclude"},
		TargetParts: join(b.liveParts, rel.Components()...),
		Rel:         rel,
		InLive:      true,
		Type:        pathx.File,
		Step:        index,
		Why: "the generated exclude, mounted over the composed tree's copy " +
			"so that git run from here ignores the workspace's names -- the " +
			"repository's own file is untouched and keeps its own content",
	})
}

// -- addressing helpers -----------------------------------------------------

// storePath mirrors a target's path inside the storage directory rather
// than escaping it into one component.
//
// Escaping the separator would blow past the 255-byte limit on a single
// name for any deep target, so the path is reproduced as a path.
func storePath(storage string, target pathx.Rel) string {
	return target.Join(storage)
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
