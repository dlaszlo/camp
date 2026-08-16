package gen_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/islands"
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

	store := fsx.Storage(built.Storage)
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
