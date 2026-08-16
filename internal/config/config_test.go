package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/testenv"
)

// rules returns the identifiers of every rule a parse refused with,
// failing the test if the error was not a refusal at all.
func rules(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		t.Fatal("expected the configuration to be refused, and it was accepted")
	}
	var list refusal.List
	if errors.As(err, &list) {
		return list.Rules()
	}
	var single refusal.R
	if errors.As(err, &single) {
		return []string{single.Rule}
	}
	t.Fatalf("expected a refusal, got a plain error: %v", err)
	return nil
}

func mustRefuse(t *testing.T, err error, rule string) refusal.List {
	t.Helper()
	found := rules(t, err)
	for _, name := range found {
		if name == rule {
			var list refusal.List
			errors.As(err, &list)
			return list
		}
	}
	t.Fatalf("expected the rule %q to fire; the rules that did were %v\n\n%v",
		rule, found, err)
	return nil
}

func TestTargetConfigurationParses(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")

	if cfg.Env != env.Path {
		t.Errorf("env: resolved to %s, wanted %s", cfg.Env, env.Path)
	}
	if cfg.Live() != env.Live {
		t.Errorf("the composed tree resolved to %s, wanted %s", cfg.Live(), env.Live)
	}
	if cfg.Upper != "code" || len(cfg.Lower) != 1 || cfg.Lower[0] != "workspace" {
		t.Errorf("the layers came out as lower=%v upper=%q", cfg.Lower, cfg.Upper)
	}
	if cfg.Identity != config.Ambient {
		t.Errorf("identity came out as %q, wanted the default", cfg.Identity)
	}
	if !cfg.AllowsOverlap(".gitignore") {
		t.Error("allow_overlap did not carry .gitignore")
	}
}

// The order of steps: is the mount order. If parsing did not preserve it,
// every ordering rule below would be checking a sequence nobody wrote.
func TestStepsKeepTheirOrder(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")

	want := []config.StepKind{config.MountRW, config.MountIslands, config.GitExclude}
	if len(cfg.Steps) != len(want) {
		t.Fatalf("parsed %d steps, wanted %d", len(cfg.Steps), len(want))
	}
	for index, kind := range want {
		if cfg.Steps[index].Kind != kind {
			t.Errorf("step %d is %q, wanted %q", index+1, cfg.Steps[index].Kind, kind)
		}
	}
	if got := len(cfg.Steps[0].Entries); got != 2 {
		t.Errorf("the first step carries %d entries, wanted 2", got)
	}
	if source := cfg.Steps[0].Entries[0].Source; source == nil || source.String() != "code/.git" {
		t.Errorf("the first entry's source came out as %v", source)
	}
}

func TestUnknownKeyIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	_, err := env.TryConfig(env.YAML() + "\nwritable: [x]\n")
	mustRefuse(t, err, "config-syntax")
}

func TestUnknownStepKindIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	_, err := env.TryConfig(env.YAML() + "  - mount_sideways:\n      - { source: \"registry\", target: \"x\" }\n")
	list := mustRefuse(t, err, "step-unknown-kind")
	if !strings.Contains(list.Error(), "mount_islands") {
		t.Error("the refusal should list the kinds camp does know")
	}
}

// A step with several keys has no defined meaning: there is no telling
// which key the step is. camp refuses rather than picking one.
func TestMultiKeyStepIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	_, err := env.TryConfig(env.YAML() +
		"  - mount_rw:\n      - { source: \"registry\", target: \"x\" }\n" +
		"    mount_ro:\n      - { source: \"registry\", target: \"y\" }\n")
	mustRefuse(t, err, "step-shape")
}

func TestBareKindThatNeedsArgumentsIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	_, err := env.TryConfig(env.YAML() + "  - mount_rw\n")
	mustRefuse(t, err, "step-needs-arguments")
}

func TestTwoGenerationStepsAreRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	_, err := env.TryConfig(env.YAML() +
		"  - generate: { command: [\"/bin/true\"] }\n")
	list := mustRefuse(t, err, "generation-steps-several")
	if !strings.Contains(list.Error(), "at most one") {
		t.Error("the refusal should say a configuration may have at most one")
	}
}

func TestPathLanguageRefusals(t *testing.T) {
	env := testenv.NewEnv(t)
	cases := []struct {
		name string
		tail string
	}{
		{"absolute target", "  - mount_rw:\n      - { source: \"registry\", target: \"/etc\" }\n"},
		{"dot-dot component", "  - mount_rw:\n      - { source: \"registry\", target: \"../escape\" }\n"},
		{"empty component", "  - mount_rw:\n      - { source: \"registry\", target: \"a//b\" }\n"},
		{"absolute source", "  - mount_rw:\n      - { source: \"/etc\", target: \"x\" }\n"},
		{"unknown repository", "  - mount_rw:\n      - { source: \"nowhere/x\", target: \"x\" }\n"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := env.TryConfig(env.YAML() + test.tail)
			mustRefuse(t, err, "path-language")
		})
	}
}

func TestDuplicateRepositoryNameIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	yaml := strings.Replace(env.YAML(),
		`  - { name: registry,  path: registry }`,
		`  - { name: code,      path: registry }`, 1)
	_, err := env.TryConfig(yaml)
	mustRefuse(t, err, "repository-duplicate-name")
}

func TestSeveralLowersAreRefusedWithAReason(t *testing.T) {
	env := testenv.NewEnv(t)
	yaml := strings.Replace(env.YAML(), "lower: [workspace]", "lower: [workspace, registry]", 1)
	_, err := env.TryConfig(yaml)
	list := mustRefuse(t, err, "lower-several")
	if !strings.Contains(list.Error(), "later iteration") {
		t.Error("the refusal should say several lowers are a later iteration, not a mistake")
	}
}

func TestSourcelessMountROIsRefused(t *testing.T) {
	env := testenv.NewEnv(t)
	_, err := env.TryConfig(env.YAML() + "  - mount_ro:\n      - { target: \"x\" }\n")
	mustRefuse(t, err, "source-missing")
}

func TestSourcelessMountRWIsAWritableHole(t *testing.T) {
	env := testenv.NewEnv(t)
	cfg, err := env.TryConfig(env.YAML() + "  - mount_rw:\n      - { target: \".workspace\" }\n")
	if err != nil {
		t.Fatalf("a sourceless mount_rw is legal and was refused:\n%v", err)
	}
	last := cfg.Steps[len(cfg.Steps)-1]
	if last.Entries[0].Source != nil {
		t.Error("the entry should have carried no source")
	}
}

func TestIdentityRoutes(t *testing.T) {
	env := testenv.NewEnv(t)

	cfg, err := env.TryConfig(env.YAML() + "\nidentity: uidmap\n")
	if err != nil {
		t.Fatalf("identity: uidmap is a legal choice and was refused:\n%v", err)
	}
	if cfg.Identity != config.UIDMap {
		t.Errorf("identity came out as %q", cfg.Identity)
	}

	_, err = env.TryConfig(env.YAML() + "\nidentity: whatever\n")
	list := mustRefuse(t, err, "identity-unknown")
	if !strings.Contains(list.Error(), "never switches between the two on its own") {
		t.Error("the refusal should say camp never falls back between the routes")
	}
}

// Every problem in one pass. Four mistakes should cost one round of
// fixing, not four rounds of surprise.
func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	env := testenv.NewEnv(t)
	yaml := strings.Replace(env.YAML(), "lower: [workspace]", "lower: [workspace, registry]", 1)
	_, err := env.TryConfig(yaml + "  - mount_rw:\n      - { source: \"registry\", target: \"/etc\" }\n" +
		"  - nonsense\n")
	found := rules(t, err)
	if len(found) < 3 {
		t.Errorf("only %d problems were reported: %v", len(found), found)
	}
}
