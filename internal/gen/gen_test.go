package gen_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/islands"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/testenv"
)

func prepared(t *testing.T, env *testenv.Env, yaml string) (plan.Plan, gen.Output, refusal.List) {
	t.Helper()
	cfg := env.Config(t, yaml)
	built, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Empty() {
		t.Fatalf("the composition was refused before generation:\n%v", refused)
	}
	if err := os.MkdirAll(built.Work, 0o755); err != nil {
		t.Fatal(err)
	}
	out, problems := gen.Prepare(built)
	return built, out, problems
}

// The exclude is one line per workspace root name, anchored with a
// leading slash, in the shortest form that says what it means.
func TestTheExcludeIsCoarseAndAnchored(t *testing.T) {
	env := testenv.NewEnv(t)
	built, out, refused := prepared(t, env, "")
	if !refused.Empty() {
		t.Fatalf("generation was refused:\n%v", refused)
	}

	for _, pattern := range out.Patterns {
		if !strings.HasPrefix(pattern, "/") {
			t.Errorf("the pattern %q is not anchored.\nThe gate compares root "+
				"entries only, so an unanchored bare name would also hide a "+
				"same-named directory deep in the code repository, and no gate "+
				"would ever fire. The slash is the only guard for that class.",
				pattern)
		}
	}

	want := map[string]bool{
		"/CLAUDE.md": false, "/AGENTS.md": false, "/.workspace": false,
		"/.claude": false, "/.registry": false, "/.git": false,
	}
	for _, pattern := range out.Patterns {
		if _, expected := want[pattern]; expected {
			want[pattern] = true
		}
	}
	for pattern, found := range want {
		if !found {
			t.Errorf("the exclude has no line for %q; the patterns are %v",
				pattern, out.Patterns)
		}
	}

	// allow_overlap entries are never excluded: for a file the line would
	// be inert, and for a directory it would hide the code repository's
	// own untracked files.
	for _, pattern := range out.Patterns {
		if pattern == "/.gitignore" {
			t.Error("an allow_overlap entry was excluded")
		}
	}

	// Five to seven lines in the steady state, against the thousands a
	// file-level enumeration would need.
	if len(out.Patterns) > 12 {
		t.Errorf("%d patterns; the coarse shape should be a handful", len(out.Patterns))
	}
	if !strings.Contains(string(out.Exclude), gen.Marker(built.Hash)) {
		t.Error("the payload does not open camp's block with the marker line")
	}
}

// The payload is the repository's own bytes, unchanged and complete, and
// then camp's block. Verification compares the mounted file against the
// whole of it: a marker-prefix match alone would accept a payload whose
// repository half had been dropped.
func TestThePayloadKeepsTheRepositorysOwnBytes(t *testing.T) {
	env := testenv.NewEnv(t)
	own := filepath.Join(env.Code, ".git", "info", "exclude")
	testenv.Write(t, own, "# mine\n/scratch\n")

	_, out, refused := prepared(t, env, "")
	if !refused.Empty() {
		t.Fatalf("generation was refused:\n%v", refused)
	}
	if !strings.HasPrefix(string(out.Exclude), "# mine\n/scratch\n") {
		t.Errorf("the payload does not begin with the repository's own bytes:\n%s",
			out.Exclude)
	}
}

// A repository exclude that does not end in a newline gets exactly one
// inserted. Direct concatenation would fuse the marker into the last
// pattern, and both would stop meaning what they say.
func TestAMissingFinalNewlineIsSuppliedExactlyOnce(t *testing.T) {
	env := testenv.NewEnv(t)
	testenv.Write(t, filepath.Join(env.Code, ".git", "info", "exclude"), "/scratch")

	_, out, refused := prepared(t, env, "")
	if !refused.Empty() {
		t.Fatalf("generation was refused:\n%v", refused)
	}
	if !strings.HasPrefix(string(out.Exclude), "/scratch\n# camp:generated ") {
		t.Errorf("the marker did not start on its own line:\n%q",
			string(out.Exclude)[:60])
	}
	if strings.Contains(string(out.Exclude), "/scratch\n\n") {
		t.Error("more than one newline was inserted")
	}
}

func TestAnEmptyRepositoryExcludeContributesNothing(t *testing.T) {
	env := testenv.NewEnv(t)
	testenv.Write(t, filepath.Join(env.Code, ".git", "info", "exclude"), "")

	_, out, refused := prepared(t, env, "")
	if !refused.Empty() {
		t.Fatalf("generation was refused:\n%v", refused)
	}
	if !strings.HasPrefix(string(out.Exclude), "# camp:generated ") {
		t.Errorf("an empty repository exclude should contribute nothing:\n%q",
			string(out.Exclude)[:40])
	}
}

// A repository with no exclude file at all is refused with the two
// commands that repair it. camp will not create them: that would be
// writing into a repository.
func TestAMissingRepositoryExcludeIsRefusedWithBothCommands(t *testing.T) {
	env := testenv.NewEnv(t)
	if err := os.RemoveAll(filepath.Join(env.Code, ".git", "info")); err != nil {
		t.Fatal(err)
	}
	cfg := env.Config(t, "")
	built, _ := plan.Prepare(cfg, plan.Namespace)
	if built.Work == "" {
		t.Skip("the composition was already refused for the missing directory")
	}
	_, refused := gen.Prepare(built)
	if !refused.Has("exclude-missing") {
		t.Fatalf("the rules that fired were %v", refused.Rules())
	}
	if !strings.Contains(refused.Error(), "mkdir -p") {
		t.Error("the refusal should print the two commands")
	}
}

// Islands come from what the source tracks, not from its raw listing. The
// raw listing would hand out islands to the source's own runtime junk --
// which is precisely what the islands mount exists to keep out.
func TestIslandsComeFromTrackedContentAndNotFromTheRawListing(t *testing.T) {
	env := testenv.NewEnv(t)
	// Something the workspace has but does not track: its own machine-local
	// settings, exactly the shape of the file the water is there for.
	testenv.Write(t, filepath.Join(env.Workspace, ".claude", "settings.local.json"), "{}\n")

	_, out, refused := prepared(t, env, "")
	if !refused.Empty() {
		t.Fatalf("generation was refused:\n%v", refused)
	}

	entries := out.Islands[".claude"]
	names := map[string]bool{}
	for _, entry := range entries {
		names[entry.Name] = true
	}
	if !names["settings.json"] || !names["agents"] {
		t.Errorf("the tracked entries did not become islands: %v", names)
	}
	if names["settings.local.json"] {
		t.Error("an untracked runtime file became an island; it belongs in the " +
			"water, where it is machine-local and survives the session")
	}
	for _, entry := range entries {
		if entry.Name == "agents" && entry.Type != "directory" {
			t.Errorf("a directory island came out as %q", entry.Type)
		}
	}
}

// The generator's output is hostile data. Whoever can edit the
// configuration can choose the program that runs at prepare, and in the
// privileged mode the mounts that follow are made by root.
func TestHostileGeneratorOutputIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	built, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}

	expected, problems := gen.Adopt(built)
	if !problems.Empty() {
		t.Fatalf("the honest generation was refused:\n%v", problems)
	}

	cases := []struct {
		name  string
		entry islands.Entry
		rule  string
	}{
		{"a parent directory", islands.Entry{Name: "..", Type: "directory"}, "generate-islands-entry"},
		{"a path with a separator", islands.Entry{Name: "a/b", Type: "file"}, "generate-islands-entry"},
		{"an empty name", islands.Entry{Name: "", Type: "file"}, "generate-islands-entry"},
		{"a name the source does not have", islands.Entry{Name: "invented", Type: "file"}, "generate-islands-absent"},
		{"a type the entry is not", islands.Entry{Name: "agents", Type: "file"}, "generate-islands-type-mismatch"},
		{"a type camp does not mount", islands.Entry{Name: "settings.json", Type: "socket"}, "generate-islands-type"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			hostile := gen.Output{
				Exclude: expected.Exclude,
				Islands: map[string][]islands.Entry{".claude": {test.entry}},
			}
			problems := gen.Validate(built, hostile, expected.Exclude)
			if !problems.Has(test.rule) {
				t.Errorf("the rules that fired were %v, wanted %q\n%v",
					problems.Rules(), test.rule, problems.Error())
			}
		})
	}

	t.Run("a duplicate entry", func(t *testing.T) {
		hostile := gen.Output{
			Exclude: expected.Exclude,
			Islands: map[string][]islands.Entry{".claude": {
				{Name: "settings.json", Type: "file"},
				{Name: "settings.json", Type: "file"},
			}},
		}
		if problems := gen.Validate(built, hostile, expected.Exclude); !problems.Has("generate-islands-duplicate") {
			t.Errorf("the rules that fired were %v", problems.Rules())
		}
	})

	t.Run("an exclude payload missing the repository's own lines", func(t *testing.T) {
		hostile := gen.Output{
			Exclude: []byte(gen.Marker(built.Hash) + "\n/CLAUDE.md\n"),
			Islands: expected.Islands,
		}
		problems := gen.Validate(built, hostile, expected.Exclude)
		if !problems.Has("generate-exclude-mismatch") {
			t.Errorf("a payload carrying only camp's block was accepted; the "+
				"rules that fired were %v", problems.Rules())
		}
	})
}

// The scaffold manifest is what lets a second run accept camp's own
// attachment points. Without it the collision rule -- which exists to
// stop camp hiding your machine-local files -- would refuse camp's own
// objects on the second up.
func TestTheScaffoldManifestAcceptsItsOwnAttachmentPointsOnASecondRun(t *testing.T) {
	env := testenv.NewEnv(t)
	built, out, refused := prepared(t, env, "")
	if !refused.Empty() {
		t.Fatalf("the first generation was refused:\n%v", refused)
	}
	if len(out.Islands[".claude"]) == 0 {
		t.Fatal("no islands were expanded")
	}

	store := filepath.Join(built.Storage, ".claude")
	for _, entry := range out.Islands[".claude"] {
		if _, err := os.Lstat(filepath.Join(store, entry.Name)); err != nil {
			t.Fatalf("no attachment point was created for %q: %v", entry.Name, err)
		}
	}
	manifest := filepath.Join(built.Storage, ".camp-scaffold")
	if _, err := os.Lstat(manifest); err != nil {
		t.Fatalf("the manifest was not written: %v", err)
	}

	// The second run meets its own scaffolding and has to accept it.
	if _, again := gen.Prepare(built); !again.Empty() {
		t.Fatalf("the second run was refused by its own attachment points:\n%v", again)
	}
}

// A file of yours where an island wants to stand is a refusal, with both
// sides named. Hiding your content behind the repository's is the design's
// enemy, and removing it is your move, not camp's.
func TestAnUnrecordedFileInTheWaterRefusesTheIsland(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	built, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}

	store := fsx.Storage(built.Config.Root, built.Hash)
	if _, err := store.MkdirAll(".claude"); err != nil {
		t.Fatal(err)
	}
	testenv.Write(t, filepath.Join(built.Storage, ".claude", "settings.json"), "mine\n")

	_, problems := gen.Prepare(built)
	if !problems.Has("islands-collision") {
		t.Fatalf("the rules that fired were %v\n%v", problems.Rules(), problems.Error())
	}
	if !strings.Contains(problems.Error(), "your move") {
		t.Error("the refusal should say whose move it is")
	}

	// And it is left exactly where it is. This is the third of the
	// scaffold's states, and the only one no crash can produce: the
	// write-ahead order records before it creates and removes before it
	// strikes, so an object with no record was never camp's. camp cannot
	// prove otherwise, so it refuses and touches nothing.
	body, err := os.ReadFile(filepath.Join(built.Storage, ".claude", "settings.json"))
	if err != nil || string(body) != "mine\n" {
		t.Errorf("the file camp cannot account for was not left alone: %q, %v", body, err)
	}
	manifest, err := islands.LoadManifest(fsx.Storage(built.Config.Root, built.Hash))
	if err != nil {
		t.Fatal(err)
	}
	if _, recorded := manifest.Records(".claude/settings.json"); recorded {
		t.Error("camp claimed a file it did not create")
	}
}

// camp's own attachment point, once something has written to it, must not
// be mounted over either.
func TestAModifiedAttachmentPointRefusesTheIsland(t *testing.T) {
	env := testenv.NewEnv(t)
	built, out, refused := prepared(t, env, "")
	if !refused.Empty() {
		t.Fatalf("the first generation was refused:\n%v", refused)
	}
	if len(out.Islands[".claude"]) == 0 {
		t.Fatal("no islands were expanded")
	}

	testenv.Write(t, filepath.Join(built.Storage, ".claude", "settings.json"), "written\n")

	_, problems := gen.Prepare(built)
	if !problems.Has("islands-scaffold-modified") {
		t.Fatalf("the rules that fired were %v", problems.Rules())
	}
}

// The write-ahead order's first boundary: the manifest is saved and the
// process dies before the object is created.
//
// What is left is a record for something that is not there. That is the
// harmless half of the pair on purpose -- the next run meets a name it
// can account for and simply creates it -- and it is the reason the
// record goes first. The other order would leave an object in the user's
// own storage that camp could not prove was its own, and the collision
// rule would then refuse the composition on the strength of camp's own
// scaffolding.
func TestARecordedAttachmentPointThatIsNotThereIsRecreated(t *testing.T) {
	env := testenv.NewEnv(t)
	built, out, refused := prepared(t, env, "")
	if !refused.Empty() {
		t.Fatalf("the first generation was refused:\n%v", refused)
	}
	if len(out.Islands[".claude"]) == 0 {
		t.Fatal("no islands were expanded")
	}

	// The intermediate state, built by hand: the manifest claims the
	// attachment point and the object is gone.
	point := filepath.Join(built.Storage, ".claude", "settings.json")
	if err := os.Remove(point); err != nil {
		t.Fatal(err)
	}
	before, err := islands.LoadManifest(fsx.Storage(built.Config.Root, built.Hash))
	if err != nil {
		t.Fatal(err)
	}
	if _, recorded := before.Records(".claude/settings.json"); !recorded {
		t.Fatal("the fixture does not hold the state this test is about")
	}

	if _, problems := gen.Prepare(built); !problems.Empty() {
		t.Fatalf("a record with no object refused the composition:\n%v", problems)
	}
	if _, err := os.Lstat(point); err != nil {
		t.Errorf("the attachment point was not created again: %v", err)
	}
	after, err := islands.LoadManifest(fsx.Storage(built.Config.Root, built.Hash))
	if err != nil {
		t.Fatal(err)
	}
	if _, recorded := after.Records(".claude/settings.json"); !recorded {
		t.Error("camp stopped claiming an attachment point it had just created")
	}
}

// The retirement's boundary, and the same pair the other way round: the
// object is removed and the process dies before its record is struck.
//
// The object is what the invariant is about, so it goes first. What is
// left is again a record for something that is not there -- and the next
// run, meeting an entry the source no longer contributes and nothing on
// disk, strikes it and says nothing, because there is nothing to say.
func TestARecordLeftBehindByAHalfFinishedRetirementIsStruck(t *testing.T) {
	env := testenv.NewEnv(t)
	built, _, refused := prepared(t, env, "")
	if !refused.Empty() {
		t.Fatalf("the first generation was refused:\n%v", refused)
	}

	// The source stops contributing the directory, so its attachment point
	// is due for retirement.
	if err := os.RemoveAll(filepath.Join(env.Workspace, ".claude", "agents")); err != nil {
		t.Fatal(err)
	}
	testenv.Commit(t, env.Workspace, "the agents are gone")

	// And the crash: the object is already removed, the record is not yet
	// struck.
	if err := os.RemoveAll(filepath.Join(built.Storage, ".claude", "agents")); err != nil {
		t.Fatal(err)
	}

	out, problems := gen.Prepare(built)
	if !problems.Empty() {
		t.Fatalf("the run after the crash was refused:\n%v", problems)
	}
	manifest, err := islands.LoadManifest(fsx.Storage(built.Config.Root, built.Hash))
	if err != nil {
		t.Fatal(err)
	}
	if _, recorded := manifest.Records(".claude/agents"); recorded {
		t.Error("camp still claims an attachment point that is gone and that " +
			"the source no longer contributes")
	}
	if _, err := os.Lstat(filepath.Join(built.Storage, ".claude", "agents")); !os.IsNotExist(err) {
		t.Errorf("something was created again at the retired attachment point: %v", err)
	}
	if notes := strings.Join(out.Notes, "\n"); strings.Contains(notes, "agents") {
		t.Errorf("a record struck for an object that was already gone was "+
			"reported as if something had happened:\n%s", notes)
	}
}

// A code repository whose index cannot be read stops the composition
// before anything is mounted.
//
// This is CAMP-REVIEW-011's shape end to end, and it is the case a real
// machine produces rather than the one a test invents: git answers the
// frame question from .git without an index and fails the question that
// follows. Read as "tracks nothing", that failure would let a mount cover
// tracked content -- and covering tracked content is what makes git
// report those files deleted and 'git commit -a' record the deletion.
//
// Both passes are asked, because either may be the one that refuses:
// today it is the planning pass, whose tracked-content check carries the
// error out; the generation pass carries it out too, from its own.
func TestACodeRepositoryWhoseIndexCannotBeReadStopsTheComposition(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")

	index := filepath.Join(env.Code, ".git", "index")
	if err := os.Chmod(index, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(index, 0o644) })

	built, refused := plan.Prepare(cfg, plan.Namespace)
	if refused.Empty() {
		if err := os.MkdirAll(built.Work, 0o755); err != nil {
			t.Fatal(err)
		}
		_, refused = gen.Prepare(built)
	}
	if !refused.Has("git-unreadable") {
		t.Fatalf("a code repository whose index cannot be read was accepted; "+
			"the rules that fired were %v", refused.Rules())
	}
	if !strings.Contains(refused.Error(), "index file open failed") {
		t.Errorf("the refusal does not carry git's own reason:\n%s", refused.Error())
	}
}

// A composition with no generation step has no exclude at all. That is
// legal; what is not legal is being quiet about it.
func TestNoGenerationStepMeansNoExclude(t *testing.T) {
	env := testenv.NewEnv(t)
	yaml := strings.Replace(env.YAML(), "  - git_exclude\n", "", 1)
	_, out, refused := prepared(t, env, yaml)
	if !refused.Empty() {
		t.Fatalf("generation was refused:\n%v", refused)
	}
	if len(out.Exclude) != 0 {
		t.Error("a payload was produced although nothing generates one")
	}
	if len(out.Notes) == 0 {
		t.Error("the fallback to the raw listing for the islands was not said out loud")
	}
}

// A composition without a generation step is prepared as completely as
// one with it: the raw listing, the expanded checks, and the attachment
// points its islands need.
//
// It used to return the moment it found no step, so the islands were
// computed and nothing was created -- and the first file island had
// nothing to bind onto, which a bind cannot make for itself. It also
// asked git, which is the one thing this branch documents that it does
// not do: what it produces is the raw listing, runtime files included.
func TestWithoutAGenerationStepTheIslandsAreStillPrepared(t *testing.T) {
	env := testenv.NewEnv(t)
	// A file the source does not track. With a generation step it would
	// not become an island; without one it does, because a raw listing is
	// what there is.
	testenv.Write(t, filepath.Join(env.Workspace, ".claude", "settings.local.json"), "{}\n")

	yaml := strings.Replace(env.YAML(), "  - git_exclude\n", "", 1)
	built, out, refused := prepared(t, env, yaml)
	if !refused.Empty() {
		t.Fatalf("generation was refused:\n%v", refused)
	}

	entries := out.Islands[".claude"]
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	if !contains(names, "settings.local.json") {
		t.Errorf("the raw listing was not used: the islands are %v", names)
	}

	// The attachment points exist, and the file island is a file: a bind
	// cannot create its own mount point, and by the time it happens the
	// tree underneath is read-only.
	for _, entry := range entries {
		path := filepath.Join(built.Storage, ".claude", entry.Name)
		info, err := os.Lstat(path)
		if err != nil {
			t.Errorf("the island %q has no attachment point in storage: %v", entry.Name, err)
			continue
		}
		if info.IsDir() != (entry.Type == pathx.Dir) {
			t.Errorf("the attachment point for %q is the wrong kind of thing", entry.Name)
		}
	}

	// And a second run over the same storage accepts what the first one
	// made rather than refusing it as somebody else's.
	if _, problems := gen.Prepare(built); !problems.Empty() {
		t.Fatalf("a second run was refused:\n%v", problems)
	}
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

// The contract on disk is the same door for the shipped step and for an
// external one, so the shipped one cannot rely on a shortcut.
func TestTheGenerationContractIsMaterialisedOnDisk(t *testing.T) {
	env := testenv.NewEnv(t)
	built, _, refused := prepared(t, env, "")
	if !refused.Empty() {
		t.Fatalf("generation was refused:\n%v", refused)
	}

	paths := gen.PathsFor(built)
	for _, name := range []string{
		gen.LowerRootList, gen.MountTargetsList, gen.AllowOverlapList, gen.UpperExcludeCurrent,
	} {
		if _, err := os.Lstat(filepath.Join(paths.In, name)); err != nil {
			t.Errorf("the input %s was not written: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(paths.In, gen.IslandsDir, ".claude"+gen.SourceSuffix)); err != nil {
		t.Errorf("the islands source file was not written: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(paths.Out, gen.ExcludeOut)); err != nil {
		t.Errorf("the payload was not published through the contract: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(paths.Out, gen.IslandsDir, ".claude"+gen.ListSuffix)); err != nil {
		t.Errorf("the islands list was not published through the contract: %v", err)
	}
}

// A configured generator is an argv vector executed directly, with camp's
// scratch as its working directory and the contract in its environment.
func TestAnExternalGeneratorRunsUnderTheContract(t *testing.T) {
	env := testenv.NewEnv(t)

	script := filepath.Join(env.Path, "generator.sh")
	testenv.Write(t, script, `#!/bin/sh
set -e
# Prove the contract: the working directory is camp's scratch, and the
# inputs are where they were promised.
test -f "$CAMP_GEN_IN/lower-root.list"
test -n "$CAMP_LIVE"
test "$PWD" = "$(dirname "$CAMP_GEN_IN")"
mkdir -p "$CAMP_GEN_OUT/islands"
cat "$CAMP_GEN_IN/upper-exclude.current" > "$CAMP_GEN_OUT/exclude"
printf '%s\n' "$CAMP_MARKER" >> "$CAMP_GEN_OUT/exclude"
`)
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	yaml := strings.Replace(env.YAML(),
		"  - mount_islands:\n      - { source: \"workspace/.claude\", target: \".claude\" }\n", "", 1)
	yaml = strings.Replace(yaml, "  - git_exclude\n",
		"  - generate: { command: [\""+script+"\"] }\n", 1)

	_, _, refused := prepared(t, env, yaml)
	// The script writes a payload that is deliberately not camp's assembly,
	// so the hostile-data check has to refuse it. That is the point: the
	// generator ran, and nothing it produced steered a mount.
	if !refused.Has("generate-exclude-mismatch") {
		t.Fatalf("the rules that fired were %v\n%v", refused.Rules(), refused.Error())
	}
}

// The generation step keeps its own environment contract, and the
// session's declarations are no part of it.
//
// Generation runs in the prepare phase, before anything is mounted and
// before any workload exists, so there is nothing for a workload's
// environment to mean here. A generator that silently received it would
// be a second, undeclared place where a configured value steers a
// program.
func TestAGeneratorDoesNotReceiveTheSessionsEnvironment(t *testing.T) {
	env := testenv.NewEnv(t)

	seen := filepath.Join(env.Path, "generator-environment")
	script := filepath.Join(env.Path, "generator.sh")
	testenv.Write(t, script, `#!/bin/sh
env > `+seen+`
mkdir -p "$CAMP_GEN_OUT/islands"
cat "$CAMP_GEN_IN/upper-exclude.current" > "$CAMP_GEN_OUT/exclude"
`)
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	yaml := strings.Replace(env.YAML(),
		"  - mount_islands:\n      - { source: \"workspace/.claude\", target: \".claude\" }\n", "", 1)
	yaml = strings.Replace(yaml, "  - git_exclude\n",
		"  - generate: { command: [\""+script+"\"] }\n", 1)
	yaml += `
session:
  environment:
    SESSION_SENTINEL: "sentinel-value-9c1f"
`

	prepared(t, env, yaml)

	text, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the generator did not run: %v", err)
	}
	if strings.Contains(string(text), "SESSION_SENTINEL") {
		t.Errorf("the generator received the session's declared environment:\n%s", text)
	}
	for _, want := range []string{"CAMP_GEN_IN=", "CAMP_GEN_OUT=", "CAMP_ENV=", "CAMP_LIVE="} {
		if !strings.Contains(string(text), want) {
			t.Errorf("the generator's own contract is missing %q:\n%s", want, text)
		}
	}
}

// The shipped step is defined as reading git. Without git it would
// quietly become something else -- islands derived from raw directory
// listings, carrying files no repository tracks -- so it refuses instead
// of falling back.
//
// This is the one case where camp really needs git. A composition that
// lists no generation step does not, and works without it.
func TestTheShippedStepRefusesWhenGitIsNotInstalled(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	built, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}
	if err := os.MkdirAll(built.Work, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))

	_, problems := gen.Prepare(built)
	if !problems.Has("generate-git-missing") {
		t.Fatalf("the rules that fired were %v", problems.Rules())
	}
	if !strings.Contains(problems.Error(), "drop the step") {
		t.Error("the refusal should say what the alternative is")
	}
}

// The one place the coarse shape does not hold: inside an allow-listed
// directory, where the two repositories' content genuinely mixes.
//
// The root line that covers every other workspace name cannot be written
// here -- it would hide the code repository's own files in the same
// directory -- so the workspace's files inside it need lines of their
// own. Without them they are in no exclude at all: 'git status' in the
// composed tree lists them as untracked and 'git add .' stages the
// workspace's content into the code repository, which is the whole thing
// the exclude exists to stop.
func TestInsideAnAllowListedDirectoryTheExcludeNamesEachWorkspacePath(t *testing.T) {
	env := testenv.NewEnv(t)
	// A directory both repositories have, allowed on purpose: the
	// workspace contributes notes, the code repository its own file, and
	// no name is on both sides -- so the gate lets the composition start.
	testenv.Write(t, filepath.Join(env.Workspace, "shared", "env.md"), "the environment's own note\n")
	testenv.Write(t, filepath.Join(env.Workspace, "shared", "deep", "more.md"), "deeper\n")
	testenv.Write(t, filepath.Join(env.Code, "shared", "code.md"), "the product's own note\n")

	yaml := strings.Replace(env.YAML(), "allow_overlap: [.gitignore]",
		"allow_overlap: [.gitignore, shared]", 1)
	built, out, refused := prepared(t, env, yaml)
	if !refused.Empty() {
		t.Fatalf("generation was refused:\n%v", refused)
	}
	_ = built

	has := func(pattern string) bool {
		for _, got := range out.Patterns {
			if got == pattern {
				return true
			}
		}
		return false
	}
	if !has("/shared/env.md") {
		t.Errorf("the workspace's file inside the allow-listed directory is in "+
			"no exclude line: %v", out.Patterns)
	}
	if !has("/shared/deep") {
		t.Errorf("a directory only the workspace has inside an allow-listed one "+
			"should be one line for the whole subtree, so that a file born in it "+
			"mid-session is covered too: %v", out.Patterns)
	}
	if has("/shared/deep/more.md") {
		t.Errorf("the walk went inside a directory only the workspace has, "+
			"which turns a subtree line into an enumeration that goes stale: %v",
			out.Patterns)
	}
	if has("/shared") {
		t.Errorf("the allow-listed directory itself was excluded, which hides "+
			"the code repository's own files in it: %v", out.Patterns)
	}
	if has("/shared/code.md") {
		t.Errorf("the code repository's own file was excluded: %v", out.Patterns)
	}
}

// A generator's islands are compared with camp's own derivation, exactly.
//
// The two ways past the syntax checks are opposite and both matter. An
// entry left out stops being a read-only island and becomes water --
// writable machine-local storage -- so an edit that should fail loudly
// succeeds and lands in no repository. An entry added is a name that
// exists but that the source does not contribute, mounted on the
// generator's say-so, by root in the privileged mode.
func TestAGeneratorsIslandsMustMatchWhatTheSourceContributes(t *testing.T) {
	cases := []struct {
		name    string
		records string
		rule    string
		says    string
	}{
		{
			name:    "an entry left out",
			records: "file\tsettings.json\n",
			rule:    "generate-islands-missing",
			says:    "agents",
		},
		{
			name: "an entry the source does not track",
			records: "file\tsettings.json\ndirectory\tagents\n" +
				"file\tsettings.local.json\n",
			rule: "generate-islands-extra",
			says: "settings.local.json",
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			env := testenv.NewEnv(t)
			// Present in the source and not tracked by it: the source's own
			// runtime file, which is exactly what the islands mount exists to
			// keep out of the composed tree.
			testenv.Write(t, filepath.Join(env.Workspace, ".claude", "settings.local.json"), "{}\n")

			script := filepath.Join(env.Path, "generator.sh")
			testenv.Write(t, script, `#!/bin/sh
set -e
mkdir -p "$CAMP_GEN_OUT/islands"
cat "$CAMP_GEN_IN/upper-exclude.current" > "$CAMP_GEN_OUT/exclude"
printf '%s' "$RECORDS" > "$CAMP_GEN_OUT/islands/.claude.list"
`)
			if err := os.Chmod(script, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("RECORDS", test.records)

			yaml := strings.Replace(env.YAML(), "  - git_exclude\n",
				"  - generate: { command: [\""+script+"\"] }\n", 1)
			_, _, refused := prepared(t, env, yaml)

			if !refused.Has(test.rule) {
				t.Fatalf("the rules that fired were %v\n%v", refused.Rules(), refused.Error())
			}
			if !strings.Contains(refused.Error(), test.says) {
				t.Errorf("the refusal does not name %q:\n%v", test.says, refused.Error())
			}
			if !strings.Contains(refused.Error(), "camp's own list") {
				t.Errorf("the refusal does not show what camp derived itself:\n%v",
					refused.Error())
			}
		})
	}
}

// An attachment point camp cannot look at stays camp's.
//
// Retirement is about provenance: the manifest is what tells camp's own
// objects from the user's machine-local files, and a permission or an I/O
// error used to be read as "it is gone already" -- so camp disclaimed
// something that still existed, and the next run met an object nothing
// could account for and refused the composition on the strength of camp's
// own scaffolding.
func TestAnAttachmentPointCampCannotLookAtIsNotDisclaimed(t *testing.T) {
	env := testenv.NewEnv(t)
	built, _, refused := prepared(t, env, "")
	if !refused.Empty() {
		t.Fatalf("generation was refused:\n%v", refused)
	}

	// The source stops contributing the directory, so the next run would
	// retire its attachment point.
	if err := os.RemoveAll(filepath.Join(env.Workspace, ".claude", "agents")); err != nil {
		t.Fatal(err)
	}
	testenv.Commit(t, env.Workspace, "the agents are gone")

	point := filepath.Join(built.Storage, ".claude", "agents")
	if err := os.Chmod(point, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(point, 0o755) })

	out, problems := gen.Prepare(built)
	if !problems.Empty() {
		t.Fatalf("the second run was refused:\n%v", problems)
	}
	if !strings.Contains(strings.Join(out.Notes, "\n"), "could not look at it") {
		t.Errorf("nothing was said about the attachment point camp could not "+
			"look at:\n%v", out.Notes)
	}

	manifest, err := islands.LoadManifest(fsx.Storage(built.Config.Root, built.Hash))
	if err != nil {
		t.Fatal(err)
	}
	if _, recorded := manifest.Records(".claude/agents"); !recorded {
		t.Error("camp disclaimed an object it could not look at, so the next " +
			"run will meet it as content nothing can account for")
	}
}

// Interrupting camp interrupts what camp started.
//
// The generator runs in a process group of its own, so that a timeout can
// end the whole tree rather than a parent that has already forked -- and
// that is exactly why Ctrl-C does not reach it: the terminal signals
// camp's foreground group, not the generator's. camp would have exited
// while the generator and its children kept writing into camp's scratch.
//
// Measured with a real signal to a real camp, and a grandchild the
// generator left behind on purpose.
const interruptEnv = "CAMP_GEN_INTERRUPT_CONFIG"

func TestMain(m *testing.M) {
	if path := os.Getenv(interruptEnv); path != "" {
		os.Exit(generateFrom(path))
	}
	os.Exit(m.Run())
}

// generateFrom is the child: an ordinary preparation, in its own process,
// so that a test can signal it the way a person's terminal would.
func generateFrom(configPath string) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	built, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Empty() {
		fmt.Fprintln(os.Stderr, refused.Error())
		return 2
	}
	if err := os.MkdirAll(built.Work, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if _, problems := gen.Prepare(built); !problems.Empty() {
		fmt.Fprintln(os.Stderr, problems.Error())
		return 3
	}
	return 0
}

func TestInterruptingCampInterruptsTheGenerator(t *testing.T) {
	env := testenv.NewEnv(t)

	ready := filepath.Join(env.Path, "ready")
	child := filepath.Join(env.Path, "grandchild.pid")
	script := filepath.Join(env.Path, "generator.sh")
	testenv.Write(t, script, `#!/bin/sh
sleep 300 &
echo $! > "`+child+`"
echo ready > "`+ready+`"
while true; do sleep 1; done
`)
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	yaml := strings.Replace(env.YAML(), "  - git_exclude\n",
		"  - generate: { command: [\""+script+"\"] }\n", 1)
	cfg := env.Config(t, yaml)

	camp := exec.Command(os.Args[0])
	camp.Env = append(os.Environ(), interruptEnv+"="+cfg.Source)
	camp.Stdout, camp.Stderr = os.Stdout, os.Stderr
	if err := camp.Start(); err != nil {
		t.Fatal(err)
	}

	waitFor(t, ready)
	data, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}

	// SIGTERM rather than SIGINT, and the difference is measured: a
	// non-interactive shell sets SIGINT to ignore in the children it
	// starts in the background, so a correctly forwarded Ctrl-C leaves
	// that sleep running and says nothing about camp. Both signals are
	// forwarded the same way.
	if err := camp.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- camp.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = camp.Process.Kill()
		t.Fatal("camp did not end after being interrupted")
	}

	// And nothing it started is still running. A process still writing
	// into camp's scratch after camp has gone is what putting the
	// generator in its own group would otherwise have bought.
	for attempt := 0; attempt < 100; attempt++ {
		if err := syscall.Kill(grandchild, 0); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(grandchild, syscall.SIGKILL)
	t.Fatalf("the generator's own child (%d) outlived the camp that started it",
		grandchild)
}

func waitFor(t *testing.T, path string) {
	t.Helper()
	for attempt := 0; attempt < 200; attempt++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}
