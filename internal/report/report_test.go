package report_test

import (
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/report"
	"github.com/dlaszlo/camp/internal/testenv"
)

// The sentinel stands for anything an inherited variable might hold: a
// token, a path nobody meant to publish, the contents of somebody's
// keyring helper. Every test that renders something looks for it.
const sentinel = "s3cret-inherited-value"

// declaring builds a configuration whose session declares two variables:
// one that replaces a name the caller already has, one that is new, and
// both of which read something inherited.
func declaring(env *testenv.Env) string {
	return env.YAML() + `
session:
  environment:
    PATH: "$CAMP_LIVE/.workspace/bin:$PATH"
    SESSION_TOKEN: "$TEST_SENTINEL_SOURCE"
`
}

func prepared(t *testing.T, env *testenv.Env, yaml string, mode plan.Mode) plan.Plan {
	t.Helper()
	built, refused := plan.Prepare(env.Config(t, yaml), mode)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}
	return built
}

// The rendering rule the whole section rests on: literal configuration
// text is shown, because it is already in the file being described, and an
// inherited value is named rather than printed. plan and explain output is
// routinely captured -- terminals, pasted issues, agent transcripts -- and
// a token must not land in one because somebody asked what would mount.
func TestNoInheritedValueReachesPlanOrExplain(t *testing.T) {
	t.Setenv("TEST_SENTINEL_SOURCE", sentinel)
	env := testenv.NewEnv(t)
	built := prepared(t, env, declaring(env), plan.Namespace)

	for name, text := range map[string]string{
		"plan":    report.Plan(built),
		"explain": report.Explain(built),
	} {
		if strings.Contains(text, sentinel) {
			t.Errorf("%s printed an inherited value:\n%s", name, text)
		}
		if !strings.Contains(text, "<inherited PATH>") {
			t.Errorf("%s does not name the inherited insertion:\n%s", name, text)
		}
		// The camp-owned paths are the plan's own subject and do appear.
		if !strings.Contains(text, built.Live) {
			t.Errorf("%s does not show the composed tree's path", name)
		}
	}
}

// The names of the two camp-owned values, the identity route, and the
// ownership fact all belong in the block; so does the note that everything
// else is inherited and that nothing is applied before the drop.
func TestThePlansSessionBlockSaysWhatWillBeApplied(t *testing.T) {
	t.Setenv("TEST_SENTINEL_SOURCE", sentinel)
	env := testenv.NewEnv(t)
	text := report.Plan(prepared(t, env, declaring(env), plan.Namespace))

	for _, want := range []string{
		"CAMP_LIVE", "PWD", "SESSION_TOKEN",
		"replaces an inherited value", "(new)",
		"appears as nobody",
		"after camp gives the mount capability back",
	} {
		if !strings.Contains(unwrapped(text), want) {
			t.Errorf("the session block does not carry %q:\n%s", want, text)
		}
	}
}

// Mapping order carries no meaning, so two orderings of the same map are
// the same composition and have to read identically.
func TestPlanTextIsStableUnderMapOrder(t *testing.T) {
	t.Setenv("TEST_SENTINEL_SOURCE", sentinel)
	env := testenv.NewEnv(t)

	one := report.Plan(prepared(t, env, env.YAML()+`
session:
  environment:
    ALPHA: "one"
    ZULU: "two"
    MIDDLE: "$HOME"
`, plan.Namespace))
	other := report.Plan(prepared(t, env, env.YAML()+`
session:
  environment:
    MIDDLE: "$HOME"
    ZULU: "two"
    ALPHA: "one"
`, plan.Namespace))

	if one != other {
		t.Errorf("two orderings of one map produced different plans:\n%s\n---\n%s",
			one, other)
	}
}

// The privileged mode starts no session. It says so once -- not applied,
// not refused, not silently skipped -- and prints no session block.
func TestThePrivilegedModeAnnouncesTheSectionExactlyOnce(t *testing.T) {
	env := testenv.NewEnv(t)
	cases := map[string]string{
		"a populated section": env.YAML() + "\nsession:\n  environment:\n    A: \"x\"\n",
		"an empty map":        env.YAML() + "\nsession:\n  environment: {}\n",
		"identity alone":      env.YAML() + "\nsession:\n  identity: uidmap\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			built := prepared(t, env, yaml, plan.Privileged)
			for where, text := range map[string]string{
				"plan":    report.Plan(built),
				"explain": report.Explain(built),
			} {
				if count := strings.Count(text, "starts no session"); count != 1 {
					t.Errorf("%s carries the announcement %d times:\n%s", where, count, text)
				}
				if strings.Contains(text, "CAMP_LIVE =") {
					t.Errorf("%s printed a session block in the mode that starts no "+
						"session:\n%s", where, text)
				}
			}
		})
	}
}

// No section, nothing to announce. A mode that says something about a
// section that is not there would be noise, and noise is what makes the
// announcement worth reading when it does appear.
func TestNothingIsAnnouncedWithoutASection(t *testing.T) {
	env := testenv.NewEnv(t)
	built := prepared(t, env, "", plan.Privileged)
	if strings.Contains(report.Plan(built), "starts no session") {
		t.Error("a configuration with no session: section was announced anyway")
	}
}

// The narration: what happened, in order, with no inherited value in any
// line and the ownership fact riding on the identity line.
func TestTheNarrationSaysWhatHappenedAndPrintsNoValues(t *testing.T) {
	var out strings.Builder
	say := report.Narrate(&out)

	say.Locks("/env/code", "/env/live")
	say.Checked(14)
	say.Generated(true)
	say.Identity(config.Session{})
	say.Environment([]string{"GIT_SSH_COMMAND", "PATH"})
	say.Mounted(14, "/env/live")
	say.Verified("/env/live")

	text := out.String()
	order := []string{"locks:", "checked:", "generated:", "identity:",
		"environment:", "mounted:", "verified:"}
	at := 0
	for _, label := range order {
		index := strings.Index(text[at:], label)
		if index < 0 {
			t.Fatalf("%q is missing, or came out of order:\n%s", label, text)
		}
		at += index + len(label)
	}
	if !strings.Contains(unwrapped(text), report.OwnershipClause) {
		t.Errorf("the identity line does not carry the ownership fact:\n%s", text)
	}
	if !strings.Contains(text, "GIT_SSH_COMMAND, PATH") {
		t.Errorf("the environment line does not name what was applied:\n%s", text)
	}
	if strings.Contains(text, "=") {
		t.Errorf("a narration line carries a name=value pair, and these lines "+
			"name variables without printing what they hold:\n%s", text)
	}
}

// unwrapped folds a narrated paragraph back into one line, so a test can
// look for a sentence without depending on where the terminal width put a
// break in it.
func unwrapped(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// A nil narrator says nothing, so no caller has to ask whether to speak.
func TestASilentNarratorIsSafe(t *testing.T) {
	var say *report.Narrator
	say.Locks("a", "b")
	say.Identity(config.Session{})
	say.Announcement(config.Session{Present: true})
}
