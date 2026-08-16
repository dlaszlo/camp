// Package config reads the one file that states intent.
//
// Everything else camp holds is derived: the plan, the exclude, the
// inventory, the state record. This file is the only thing a person
// writes, so it is read strictly -- an unknown key, an unknown step kind
// or a path that could climb out of what it is resolved against is
// refused rather than guessed at.
//
// The shape worth understanding before reading further is `steps:`. It is
// one ordered sequence and its order is the mount order. An earlier
// design had three sibling keys -- mount_ro, mount_rw, mount_islands --
// and a rule saying the mounts run "in file order", which was not
// implementable: in YAML a sequence carries order and a mapping's keys do
// not, so there was no defined interleaving between three keys at the
// same level. One sequence makes the order true by definition, which is
// what lets the whole composition be walked on paper before anything is
// mounted.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dlaszlo/camp/internal/enc"
	"github.com/dlaszlo/camp/internal/envx"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/refusal"
)

const (
	// Dir is camp's directory inside the environment root.
	Dir = ".camp"
	// FileName is the configuration, inside that directory.
	FileName = "config.yml"
)

// Path returns the configuration's path for an environment root.
func Path(env string) string { return filepath.Join(env, Dir, FileName) }

// Config is the whole file, resolved as far as it can be without asking
// the filesystem anything except where env: really is.
type Config struct {
	// Source is the absolute path the configuration was read from.
	Source string
	// Env is the environment root: absolute, and the one path camp ever
	// resolves through symlinks.
	Env string
	// Merged is where the composed tree appears, relative to Env.
	Merged pathx.Rel

	// Repositories are the participants, by name. A set, not a sequence:
	// nothing depends on the order they are written in.
	Repositories []Repository

	// Lower and Upper name repositories. Lower stays a list while only one
	// entry is accepted, so that several lowers can arrive later without
	// the file changing shape.
	Lower []string
	Upper string

	// AllowOverlap is the set of root names permitted to exist on both
	// sides. It is the only escape hatch the gate has, and it is
	// configuration -- a decision that is recorded and diffable rather
	// than a flag typed once in anger.
	AllowOverlap []string

	// Steps is the configuration's own part of the mount sequence, in the
	// order it will run.
	Steps []Step

	// Session is everything that configures the session camp starts, and
	// nothing that configures the tree.
	Session Session
}

// Session is the `session:` section: what shapes the supervised run that
// 'camp run' and 'camp shell' create, and that ends with its last process.
//
// It exists as a section because more than one key is scoped this way, and
// because a flat key does not say in the file what it applies to. What is
// mounted, protected and generated is the composition itself -- the same
// in every mode -- and stays outside this section. The privileged mode
// starts no session, so it announces this section rather than applying it
// or refusing it (§14): an explicit statement of non-application cannot
// look applied, and refusing would only force editing the file to move
// between the modes.
type Session struct {
	// Present is whether the file has the section at all. It is what the
	// privileged mode announces on; an empty section declares nothing but is
	// still present.
	Present bool

	// Identity selects how the user is mapped inside the namespace. Empty
	// is route A, the only route camp takes on its own.
	Identity Identity

	// Environment is every declared variable, in byte order by name. The
	// mapping's own order carries no meaning: declarations never read each
	// other, so nothing depends on which was written first.
	Environment []Declaration
}

// Declaration is one environment variable the session declares.
//
// It carries the expression, never a resolved value: resolution needs the
// environment camp was started with, happens in the process that is about
// to start the workload, and the bytes it produces reach that workload's
// environment and nothing else camp writes.
type Declaration struct {
	Name string
	Expr envx.Expr
	// Line is where the entry sits in the file.
	Line int
}

// Declares reports whether the session declares any environment variable.
func (s Session) Declares() bool { return len(s.Environment) > 0 }

// Identity is the uid-mapping route for the namespace mode.
type Identity string

const (
	// Ambient is route A: the caller's own uid maps to itself and
	// CAP_SYS_ADMIN is carried in the ambient set exactly until the mounts
	// are verified. Inside, id shows the real user. This is the default,
	// and the only route camp chooses by itself.
	Ambient Identity = ""
	// UIDMap is route B: newuidmap/newgidmap and the subuid range,
	// podman's keep-id shape. It is chosen explicitly and never by
	// fallback, because the two routes present different uid worlds to
	// whatever runs inside, and a silent switch between them would change
	// what files a session creates.
	UIDMap Identity = "uidmap"
)

// Repository is one participant.
type Repository struct {
	Name string
	// Path is relative to Env.
	Path pathx.Rel
}

// StepKind is what one step of the sequence does.
type StepKind string

const (
	// MountRO binds a source read-only over a target.
	MountRO StepKind = "mount_ro"
	// MountRW binds a source writable over a target, or -- with no source
	// -- provides an empty writable hole backed by camp's storage.
	MountRW StepKind = "mount_rw"
	// MountIslands is the third mount type: a writable machine-local floor
	// over the target, with the source's contributed entries standing in
	// it read-only.
	MountIslands StepKind = "mount_islands"
	// GitExclude is the shipped generation step: it reads git and produces
	// the exclude payload and the islands expansions.
	GitExclude StepKind = "git_exclude"
	// Generate is the custom generation step: the same contract, an
	// external program instead of the built-in git reads.
	Generate StepKind = "generate"
)

// Kinds lists every step kind camp knows, for the refusal message that
// has to name them.
func Kinds() []StepKind {
	return []StepKind{MountRO, MountRW, MountIslands, GitExclude, Generate}
}

// IsMount reports whether a kind carries mount entries.
func (k StepKind) IsMount() bool {
	return k == MountRO || k == MountRW || k == MountIslands
}

// Generates reports whether a kind produces artefacts in the prepare
// phase. At most one such step may appear in a configuration: there is
// one exclude payload, and two steps claiming it cannot both be right.
func (k StepKind) Generates() bool {
	return k == GitExclude || k == Generate
}

// Step is one item of the sequence.
type Step struct {
	Kind StepKind
	// Index is the step's position in the file, counted from zero, for
	// messages that have to say which step.
	Index int
	// Line is where the step starts in the file.
	Line int

	// Entries is the mount list, for a mount kind.
	Entries []Entry

	// Command is the argv vector, for a custom generation step. It is
	// executed directly -- never through a shell, so no word splitting and
	// no expansion happen between the file and the process.
	Command []string
	// Timeout kills the generator's process group when it expires. Zero
	// means no timeout, which is the default: camp is driven from a
	// terminal by a person who can interrupt it.
	Timeout time.Duration
}

// Entry is one {source, target} pair.
type Entry struct {
	// Source is nil for a sourceless mount_rw -- a plain writable hole.
	Source *Source
	Target pathx.Rel
	// Line is where the entry sits in the file.
	Line int
}

// Source addresses a place inside a repository.
type Source struct {
	Repository string
	// Path is relative to that repository's root, and empty for the root
	// itself.
	Path pathx.Rel
	// Raw is what was written, for messages.
	Raw string
}

// String renders a source the way it was addressed.
func (s Source) String() string {
	if s.Path.Empty() {
		return s.Repository
	}
	return s.Repository + "/" + s.Path.String()
}

// Repository returns one participant by name.
func (c Config) Repository(name string) (Repository, bool) {
	for _, repo := range c.Repositories {
		if repo.Name == name {
			return repo, true
		}
	}
	return Repository{}, false
}

// RepositoryPath returns a participant's absolute path.
func (c Config) RepositoryPath(name string) string {
	repo, ok := c.Repository(name)
	if !ok {
		return ""
	}
	return repo.Path.Join(c.Env)
}

// Live is the absolute path of the composed tree.
func (c Config) Live() string { return c.Merged.Join(c.Env) }

// UpperPath is the absolute path of the code repository.
func (c Config) UpperPath() string { return c.RepositoryPath(c.Upper) }

// LowerPath is the absolute path of the workspace repository.
//
// Exactly one lower is accepted today; the list shape is what lets a
// second arrive later without the file changing.
func (c Config) LowerPath() string {
	if len(c.Lower) == 0 {
		return ""
	}
	return c.RepositoryPath(c.Lower[0])
}

// IsLower reports whether a repository is a lower layer. A mount_rw may
// not source from one: the lower is never written, by any route.
func (c Config) IsLower(name string) bool {
	for _, lower := range c.Lower {
		if lower == name {
			return true
		}
	}
	return false
}

// AllowsOverlap reports whether a root name is permitted on both sides.
func (c Config) AllowsOverlap(name string) bool {
	for _, allowed := range c.AllowOverlap {
		if allowed == name {
			return true
		}
	}
	return false
}

// GenerationStep returns the configuration's generation step, if it has
// one. A composition without one has no exclude at all, which plan says
// plainly rather than leaving the defence out silently.
func (c Config) GenerationStep() (Step, bool) {
	for _, step := range c.Steps {
		if step.Kind.Generates() {
			return step, true
		}
	}
	return Step{}, false
}

// CampDir is $ENV/.camp.
func (c Config) CampDir() string { return filepath.Join(c.Env, Dir) }

// -- reading ---------------------------------------------------------------

// Find walks upward from a directory looking for .camp/config.yml, the
// way git finds a repository, so that inside a composition no path has to
// be typed.
func Find(start string) (string, error) {
	for directory := start; ; directory = filepath.Dir(directory) {
		candidate := filepath.Join(directory, Dir, FileName)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
		if directory == filepath.Dir(directory) {
			break
		}
	}
	return "", fmt.Errorf("no %s/%s here or in any parent directory of %s.\n"+
		"Run 'camp init' in the environment directory to write one, or point at "+
		"an existing one with -f", Dir, FileName, start)
}

// Load reads and parses a configuration from disk.
func Load(path string) (Config, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("resolving %s: %w", path, err)
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		return Config{}, fmt.Errorf("reading the configuration %s: %w", absolute, err)
	}
	return Parse(data, absolute)
}

// raw is the file as YAML sees it, before any of it means anything.
type raw struct {
	Env          string      `yaml:"env"`
	Merged       string      `yaml:"merged"`
	Repositories []rawRepo   `yaml:"repositories"`
	Overlayfs    rawOverlay  `yaml:"overlayfs"`
	AllowOverlap []string    `yaml:"allow_overlap"`
	Steps        []yaml.Node `yaml:"steps"`
	// Identity is the key's old top-level position. It is still read, not
	// to honour it but to recognise it: a shipped key that moves owes the
	// reader its forwarding address rather than the generic unknown-key
	// message.
	Identity string      `yaml:"identity"`
	Session  *rawSession `yaml:"session"`
}

// rawSession is the section as YAML sees it. The fields are nodes rather
// than values so that a wrong shape is refused with its own message and
// its own line, instead of failing the whole document's decode.
type rawSession struct {
	Identity    string    `yaml:"identity"`
	Environment yaml.Node `yaml:"environment"`
}

type rawRepo struct {
	Name string `yaml:"name"`
	Path string `yaml:"path"`
}

type rawOverlay struct {
	Lower []string `yaml:"lower"`
	Upper string   `yaml:"upper"`
}

type rawEntry struct {
	Source string `yaml:"source"`
	Target string `yaml:"target"`
}

type rawGenerate struct {
	Command []string `yaml:"command"`
	Timeout int      `yaml:"timeout"`
}

// Parse reads the configuration bytes.
//
// Every problem the file has is reported, not the first one: a
// configuration with four mistakes should cost one round of fixing, not
// four rounds of surprise. Only a file YAML itself cannot read stops
// early, because after that there is nothing left to check.
func Parse(data []byte, source string) (Config, error) {
	var document raw
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return Config{}, refusal.New("config-syntax",
			"%s could not be read as YAML: %v.\n"+
				"Unknown keys are refused too -- camp never guesses at a key it does "+
				"not know, because a typo that is quietly ignored is a protection "+
				"that quietly is not there.", source, err)
	}

	cfg := Config{Source: source}
	var refused refusal.List

	cfg.Env = parseEnv(document.Env, &refused)
	cfg.Merged = parseMerged(document.Merged, &refused)
	cfg.Repositories = parseRepositories(document.Repositories, &refused)
	cfg.Lower, cfg.Upper = parseOverlay(document.Overlayfs, cfg.Repositories, &refused)
	cfg.AllowOverlap = parseAllowOverlap(document.AllowOverlap, &refused)
	checkMovedIdentity(document.Identity, &refused)
	cfg.Session = parseSession(document.Session, &refused)
	cfg.Steps = parseSteps(document.Steps, cfg.Repositories, &refused)

	checkGenerationSteps(cfg.Steps, &refused)

	return cfg, refused.Err()
}

func parseEnv(value string, refused *refusal.List) string {
	if value == "" {
		refused.Add("env-missing",
			"env: is missing. It names the directory the repositories and the "+
				"composed tree live in, and it is the one absolute path in the "+
				"file -- every other path is relative to it.")
		return ""
	}
	expanded, err := pathx.ExpandHome(value)
	if err != nil {
		refused.Add("env-home", "%v", err)
		return ""
	}
	if !filepath.IsAbs(expanded) {
		refused.Add("env-relative",
			"env: is %q, which is relative. It has to be an absolute path, or "+
				"start with ~/ -- camp resolves it once at startup and addresses "+
				"everything else beneath it, so it cannot depend on where the "+
				"command was run from.", value)
		return ""
	}
	real, err := pathx.Real(expanded)
	if err != nil {
		refused.Add("env-missing-dir",
			"env: is %s, which camp could not resolve: %v.\n"+
				"Create the directory, or correct the path.", expanded, err)
		return expanded
	}
	return real
}

func parseMerged(value string, refused *refusal.List) pathx.Rel {
	if value == "" {
		refused.Add("merged-missing",
			"merged: is missing. It names the directory the composed tree "+
				"appears in, relative to env: -- for example 'project-live'.")
		return pathx.Rel{}
	}
	rel, err := pathx.ParseRel("merged:", value)
	if err != nil {
		refused.Add("path-language", "%v", err)
		return pathx.Rel{}
	}
	return rel
}

func parseRepositories(entries []rawRepo, refused *refusal.List) []Repository {
	if len(entries) == 0 {
		refused.Add("repositories-missing",
			"repositories: is empty. At least the two the overlay needs have to "+
				"be listed -- the workspace as the lower and the code repository "+
				"as the upper.")
		return nil
	}

	seen := map[string]bool{}
	repositories := make([]Repository, 0, len(entries))
	for _, entry := range entries {
		name, err := pathx.ParseComponent("a repository name", entry.Name)
		if err != nil {
			refused.Add("path-language", "%v", err)
			continue
		}
		if seen[name] {
			refused.Add("repository-duplicate-name",
				"two repositories are both called %q. The name is how sources "+
					"address a repository, so it has to identify one.", name)
			continue
		}
		seen[name] = true

		path, err := pathx.ParseRel(fmt.Sprintf("the path of repository %q", name), entry.Path)
		if err != nil {
			refused.Add("path-language", "%v", err)
			continue
		}
		repositories = append(repositories, Repository{Name: name, Path: path})
	}
	return repositories
}

func parseOverlay(overlay rawOverlay, repositories []Repository, refused *refusal.List) ([]string, string) {
	known := func(name string) bool {
		for _, repo := range repositories {
			if repo.Name == name {
				return true
			}
		}
		return false
	}

	switch {
	case len(overlay.Lower) == 0:
		refused.Add("lower-missing",
			"overlayfs.lower is empty. It names the workspace repository -- the "+
				"read-only layer the composed tree stands on. Write it as a list "+
				"with one entry: 'lower: [workspace]'.")
	case len(overlay.Lower) > 1:
		refused.Add("lower-several",
			"overlayfs.lower names %d repositories (%s). camp accepts exactly "+
				"one today.\n"+
				"Several lowers are a later iteration: they will be merged by camp "+
				"into one read-only tree, because file shadowing between read-only "+
				"layers is silent whoever merges them and has to be reported by "+
				"name. Until that exists, name one.",
			len(overlay.Lower), strings.Join(overlay.Lower, ", "))
	}
	for _, name := range overlay.Lower {
		if !known(name) {
			refused.Add("lower-unknown",
				"overlayfs.lower names %q, which is not in repositories:. A layer "+
					"is addressed by the name a repository was given there.", name)
		}
	}

	if overlay.Upper == "" {
		refused.Add("upper-missing",
			"overlayfs.upper is missing. It names the code repository -- the only "+
				"place ordinary writes in the composed tree land.")
	} else if !known(overlay.Upper) {
		refused.Add("upper-unknown",
			"overlayfs.upper is %q, which is not in repositories:.", overlay.Upper)
	}

	for _, lower := range overlay.Lower {
		if lower == overlay.Upper && lower != "" {
			refused.Add("upper-is-lower",
				"%q is named as both the lower and the upper. A repository cannot "+
					"be the layer underneath and the layer on top: the whole point of "+
					"the arrangement is that a write reaching the upper never touches "+
					"the lower.", lower)
		}
	}

	return overlay.Lower, overlay.Upper
}

func parseAllowOverlap(entries []string, refused *refusal.List) []string {
	names := make([]string, 0, len(entries))
	seen := map[string]bool{}
	for _, entry := range entries {
		name, err := pathx.ParseComponent("an allow_overlap entry", entry)
		if err != nil {
			refused.Add("path-language", "%v", err)
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// checkMovedIdentity meets a configuration written before the key moved.
//
// identity: configures the session and nothing else, so it now lives in
// the session: section beside the environment. Left as an unknown
// top-level key it would produce the generic "camp does not know this
// key" refusal, which is true and useless: the reader knows what the key
// means and needs to be told where it went.
func checkMovedIdentity(value string, refused *refusal.List) {
	if value == "" {
		return
	}
	refused.Add("identity-moved",
		"identity: is at the top level of the file, and it now lives inside "+
			"the session: section:\n\n"+
			"  session:\n    identity: %s\n\n"+
			"It configures the session 'camp run' and 'camp shell' start -- which "+
			"uid route their namespace uses -- and nothing about the composed "+
			"tree, so it sits with the other key of that kind rather than beside "+
			"the mounts. Nothing else about it changed.", value)
}

// parseSession reads the section that configures the session.
func parseSession(section *rawSession, refused *refusal.List) Session {
	if section == nil {
		return Session{}
	}
	return Session{
		Present:     true,
		Identity:    parseIdentity(section.Identity, refused),
		Environment: parseEnvironment(section.Environment, refused),
	}
}

func parseIdentity(value string, refused *refusal.List) Identity {
	switch Identity(value) {
	case Ambient:
		return Ambient
	case UIDMap:
		return UIDMap
	default:
		refused.Add("identity-unknown",
			"session.identity is %q, which camp does not know. Leave it out for "+
				"the default -- your own uid mapped to itself, with the mount "+
				"capability carried in the ambient set and dropped before anything "+
				"runs -- or write 'identity: uidmap' to use newuidmap and the "+
				"subuid range instead. camp never switches between the two on its "+
				"own: they present different uid worlds to whatever runs inside.",
			value)
		return Ambient
	}
}

// parseEnvironment reads session.environment: the variables the workload
// receives.
//
// Note which key this is not. The top-level env: names the environment
// *root directory*, the one absolute path in the file; this declares the
// *process environment* a session's workload runs with. The two are
// neighbours in name and nothing else.
//
// Every entry is checked here except one thing: whether a referenced name
// is set. That needs the environment the command was started with, so it
// belongs to planning, and everything else is reported in this one pass.
func parseEnvironment(node yaml.Node, refused *refusal.List) []Declaration {
	if node.Kind == 0 {
		return nil
	}
	if node.Kind != yaml.MappingNode {
		refused.Add("environment-shape",
			"session.environment at line %d is %s. It has to be a mapping from "+
				"variable names to string values:\n\n"+
				"  session:\n    environment:\n      NAME: \"value\"\n\n"+
				"camp does not read another YAML shape as process settings.",
			node.Line, describeNode(node))
		return nil
	}

	declarations := make([]Declaration, 0, len(node.Content)/2)
	lines := map[string]int{}
	for index := 0; index+1 < len(node.Content); index += 2 {
		key, value := node.Content[index], node.Content[index+1]
		name := key.Value

		if err := envx.CheckName(name); err != nil {
			push(refused, err)
			continue
		}
		if previous, duplicate := lines[name]; duplicate {
			refused.Add("environment-duplicate",
				"session.environment declares %s twice, at lines %d and %d.\n"+
					"Only one of them could reach the workload, and camp will not "+
					"pick which. Keep the one you meant.",
				enc.Encode(name), previous, key.Line)
			continue
		}
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			refused.Add("environment-shape",
				"session.environment.%s at line %d is %s, not a string.\n"+
					"An environment value is bytes: it has no number, boolean or null "+
					"type for camp to convert from, and guessing at the text a value "+
					"was meant to have is not something camp does. Quote it: "+
					"%s: \"%s\".",
				enc.Encode(name), value.Line, describeNode(*value),
				enc.Encode(name), enc.Encode(value.Value))
			continue
		}

		expression, err := envx.Parse(name, value.Value)
		if err != nil {
			push(refused, err)
			continue
		}
		lines[name] = key.Line
		declarations = append(declarations, Declaration{
			Name: name, Expr: expression, Line: key.Line,
		})
	}

	// Byte order, so that two orderings of the same map are the same
	// composition and every report of it reads the same.
	sort.Slice(declarations, func(i, j int) bool {
		return declarations[i].Name < declarations[j].Name
	})
	return declarations
}

// describeNode names a YAML shape the way somebody reading the file would.
func describeNode(node yaml.Node) string {
	switch node.Kind {
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.AliasNode:
		return "an alias"
	}
	switch node.Tag {
	case "!!null":
		return "empty"
	case "!!int", "!!float":
		return "a number"
	case "!!bool":
		return "a true/false value"
	case "!!str":
		return "a string"
	default:
		return "a " + strings.TrimPrefix(node.Tag, "!!") + " value"
	}
}

// push carries a refusal built elsewhere into this file's list, so that a
// grammar check living in its own package still reports through the one
// mechanism everything else does.
func push(refused *refusal.List, err error) {
	var single refusal.R
	if errors.As(err, &single) {
		refused.Push(single)
		return
	}
	refused.Add("environment-shape", "%v", err)
}

// parseSteps reads the sequence. Each item is either a bare kind or a
// single-key mapping from kind to arguments; anything else is refused
// rather than interpreted.
func parseSteps(nodes []yaml.Node, repositories []Repository, refused *refusal.List) []Step {
	steps := make([]Step, 0, len(nodes))
	for index, node := range nodes {
		step, ok := parseStep(index, node, repositories, refused)
		if ok {
			steps = append(steps, step)
		}
	}
	return steps
}

func parseStep(index int, node yaml.Node, repositories []Repository, refused *refusal.List) (Step, bool) {
	step := Step{Index: index, Line: node.Line}

	switch node.Kind {
	case yaml.ScalarNode:
		kind, ok := knownKind(node.Value, index, node.Line, refused)
		if !ok {
			return Step{}, false
		}
		if kind.IsMount() || kind == Generate {
			refused.Add("step-needs-arguments",
				"step %d (line %d) is the bare kind %q, which needs arguments.\n"+
					"A mount kind takes a list of {source, target} entries; generate "+
					"takes {command: [...]}. Only git_exclude stands alone.",
				index+1, node.Line, node.Value)
			return Step{}, false
		}
		step.Kind = kind
		return step, true

	case yaml.MappingNode:
		if len(node.Content) != 2 {
			refused.Add("step-shape",
				"step %d (line %d) is a mapping with %d keys. A step is either a "+
					"bare kind -- '- git_exclude' -- or one kind with its arguments "+
					"-- '- mount_rw: [...]'. With several keys there is no telling "+
					"which one the step is, and camp mounts nothing on a guess.",
				index+1, node.Line, len(node.Content)/2)
			return Step{}, false
		}
		kind, ok := knownKind(node.Content[0].Value, index, node.Line, refused)
		if !ok {
			return Step{}, false
		}
		step.Kind = kind
		switch {
		case kind.IsMount():
			step.Entries = parseEntries(index, kind, node.Content[1], repositories, refused)
			if len(step.Entries) == 0 {
				return Step{}, false
			}
		case kind == Generate:
			if !parseGenerate(index, node.Content[1], &step, refused) {
				return Step{}, false
			}
		default:
			refused.Add("step-takes-no-arguments",
				"step %d (line %d) is %q with arguments, and %q takes none.",
				index+1, node.Line, string(kind), string(kind))
			return Step{}, false
		}
		return step, true

	default:
		refused.Add("step-shape",
			"step %d (line %d) is neither a name nor a mapping. A step is either "+
				"a bare kind -- '- git_exclude' -- or one kind with its arguments "+
				"-- '- mount_rw: [...]'.", index+1, node.Line)
		return Step{}, false
	}
}

func knownKind(value string, index, line int, refused *refusal.List) (StepKind, bool) {
	for _, kind := range Kinds() {
		if string(kind) == value {
			return kind, true
		}
	}
	names := make([]string, 0, len(Kinds()))
	for _, kind := range Kinds() {
		names = append(names, string(kind))
	}
	refused.Add("step-unknown-kind",
		"step %d (line %d) is %q, which is not a kind camp knows. The kinds "+
			"are: %s.\n"+
			"Nothing camp does not recognise is mounted on a guess.",
		index+1, line, value, strings.Join(names, ", "))
	return "", false
}

func parseEntries(index int, kind StepKind, node *yaml.Node, repositories []Repository, refused *refusal.List) []Entry {
	var rawEntries []rawEntry
	if err := node.Decode(&rawEntries); err != nil {
		refused.Add("step-entries",
			"the entries of step %d (%s, line %d) could not be read: %v.\n"+
				"A mount kind takes a list of mappings, each with a target and -- "+
				"except for a sourceless mount_rw -- a source.",
			index+1, kind, node.Line, err)
		return nil
	}
	if len(rawEntries) == 0 {
		refused.Add("step-entries",
			"step %d (%s, line %d) has no entries. A mount step that mounts "+
				"nothing is a line that does nothing, and camp will not carry it "+
				"as if it did.", index+1, kind, node.Line)
		return nil
	}

	entries := make([]Entry, 0, len(rawEntries))
	for _, item := range rawEntries {
		entry := Entry{Line: node.Line}
		target, err := pathx.ParseRel(
			fmt.Sprintf("the target of a %s entry in step %d", kind, index+1), item.Target)
		if err != nil {
			refused.Add("path-language", "%v", err)
			continue
		}
		entry.Target = target

		if item.Source == "" {
			if kind != MountRW {
				refused.Add("source-missing",
					"the %s entry for target %q in step %d has no source. Only "+
						"mount_rw may stand without one -- that form is a plain "+
						"writable hole, backed by camp's own storage and starting "+
						"empty. A %s with nothing to mount has no meaning.",
					kind, target.String(), index+1, kind)
				continue
			}
			entries = append(entries, entry)
			continue
		}

		source, err := parseSource(item.Source, repositories)
		if err != nil {
			refused.Add("path-language", "%v", err)
			continue
		}
		entry.Source = &source
		entries = append(entries, entry)
	}
	return entries
}

// parseSource reads "<repository>/<path>", or a bare repository name for
// its root.
func parseSource(raw string, repositories []Repository) (Source, error) {
	field := fmt.Sprintf("the source %q", raw)
	if strings.HasPrefix(raw, "/") {
		return Source{}, fmt.Errorf("%s is an absolute path. A source names a "+
			"repository and a path inside it -- 'code/.git', or just 'registry' "+
			"for the repository's own root. camp mounts nothing it cannot "+
			"attribute to a declared repository", field)
	}

	name, rest, _ := strings.Cut(raw, "/")
	component, err := pathx.ParseComponent(
		fmt.Sprintf("the repository name in source %q", raw), name)
	if err != nil {
		return Source{}, err
	}

	found := false
	for _, repo := range repositories {
		if repo.Name == component {
			found = true
			break
		}
	}
	if !found {
		known := make([]string, 0, len(repositories))
		for _, repo := range repositories {
			known = append(known, repo.Name)
		}
		return Source{}, fmt.Errorf("%s starts with %q, which is not in "+
			"repositories:. The repositories declared are: %s",
			field, component, strings.Join(known, ", "))
	}

	source := Source{Repository: component, Raw: raw}
	if rest != "" {
		path, err := pathx.ParseRel(fmt.Sprintf("the path in source %q", raw), rest)
		if err != nil {
			return Source{}, err
		}
		source.Path = path
	} else if strings.Contains(raw, "/") {
		return Source{}, fmt.Errorf("%s ends in a slash. Write the repository "+
			"name alone to mean its root", field)
	}
	return source, nil
}

func parseGenerate(index int, node *yaml.Node, step *Step, refused *refusal.List) bool {
	var arguments rawGenerate
	if err := node.Decode(&arguments); err != nil {
		refused.Add("step-generate",
			"the arguments of the generate step %d (line %d) could not be read: "+
				"%v.\nIt takes {command: [\"prog\", \"arg\"], timeout: <seconds>}.",
			index+1, node.Line, err)
		return false
	}
	if len(arguments.Command) == 0 {
		refused.Add("step-generate",
			"the generate step %d (line %d) has no command. It takes an argument "+
				"vector -- {command: [\"prog\", \"arg\"]} -- which camp executes "+
				"directly. There is no shell in between, so nothing is split on "+
				"spaces and nothing is expanded.", index+1, node.Line)
		return false
	}
	if arguments.Timeout < 0 {
		refused.Add("step-generate",
			"the generate step %d (line %d) has a negative timeout. Leave it out "+
				"for no timeout, which is the default.", index+1, node.Line)
		return false
	}
	step.Command = arguments.Command
	step.Timeout = time.Duration(arguments.Timeout) * time.Second
	return true
}

// checkGenerationSteps enforces the one-generation-step rule.
//
// There is one exclude payload. Two steps claiming it cannot both be
// right, and camp will not pick a winner.
func checkGenerationSteps(steps []Step, refused *refusal.List) {
	var generating []Step
	for _, step := range steps {
		if step.Kind.Generates() {
			generating = append(generating, step)
		}
	}
	if len(generating) < 2 {
		return
	}
	names := make([]string, 0, len(generating))
	for _, step := range generating {
		names = append(names, fmt.Sprintf("%s (step %d, line %d)",
			step.Kind, step.Index+1, step.Line))
	}
	refused.Add("generation-steps-several",
		"the configuration has %d generation steps: %s.\n"+
			"A configuration may have at most one. Both would produce the exclude "+
			"payload and the islands expansions, there is only one of each, and "+
			"two steps claiming them cannot both be right -- so camp refuses "+
			"rather than picking a winner. Keep the one you meant.",
		len(generating), strings.Join(names, " and "))
}
