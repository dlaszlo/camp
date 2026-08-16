package report_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
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

// The skeleton and the example teach the grammar, so what they teach has
// to be true: the block has to uncomment into something camp reads. A
// commented example nobody ever parses is exactly where a stale one hides.
func TestTheCommentedSessionBlocksUncommentIntoSomethingCampReads(t *testing.T) {
	root := testenv.RepoRoot(t)
	example, err := os.ReadFile(filepath.Join(root, "examples", "config.yml"))
	if err != nil {
		t.Fatal(err)
	}

	for name, text := range map[string]string{
		"the camp init skeleton": report.ConfigTemplate("/home/you/work"),
		"examples/config.yml":    string(example),
	} {
		t.Run(name, func(t *testing.T) {
			// The four things A5 and the build plan ask every introduction
			// of the key to carry: the grammar, the distinction from env:,
			// what the other mode does with the section, and where the
			// worked recipe is.
			for _, want := range []string{
				"session:", "environment:", "$CAMP_LIVE",
				"root directory", "process environment",
				"camp up", "docs/install.md",
			} {
				if !strings.Contains(unwrapped(text), want) {
					t.Errorf("%s does not carry %q", name, want)
				}
			}

			// As shipped: comments, and a file that parses.
			if _, err := config.Parse([]byte(text), "config.yml"); err != nil {
				assertNoSessionRule(t, err, "as shipped")
			}
			// And with the block uncommented, exactly as the file tells the
			// reader to do it.
			if _, err := config.Parse([]byte(uncomment(t, text)), "config.yml"); err != nil {
				assertNoSessionRule(t, err, "with the block uncommented")
			}
		})
	}
}

// uncomment strips one leading '# ' from everything after the '# session:'
// line, which is what "uncomment from here down" means.
func uncomment(t *testing.T, text string) string {
	t.Helper()
	head, block, found := strings.Cut(text, "# session:\n")
	if !found {
		t.Fatal("there is no commented session: block to uncomment")
	}
	lines := []string{head + "session:"}
	for _, line := range strings.Split(block, "\n") {
		lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(line, "#"), " "))
	}
	return strings.Join(lines, "\n")
}

// assertNoSessionRule fails on any refusal about the section. The
// skeletons name directories that do not exist, so path refusals are
// expected and are not what this is about.
func assertNoSessionRule(t *testing.T, err error, when string) {
	t.Helper()
	var list refusal.List
	if !errors.As(err, &list) {
		t.Fatalf("%s: %v", when, err)
	}
	for _, rule := range list.Rules() {
		if strings.HasPrefix(rule, "environment-") || strings.HasPrefix(rule, "identity-") ||
			rule == "config-syntax" {
			t.Errorf("%s, the session block was refused (%s):\n%v", when, rule, err)
		}
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

	// Every line says what it is, in the first column. Without that a run
	// is one block in which the step that failed looks like the ones that
	// did not.
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if strings.HasPrefix(line, " ") {
			continue // a folded continuation, aligned under its own text
		}
		if !strings.HasPrefix(line, report.MarkOK) {
			t.Errorf("a line begins with something other than a marker: %q", line)
		}
	}
}

// A failure is marked as one, and a note as a note. The three read in the
// same column so the eye finds the one that is not [OK].
func TestWhatIsWrongIsMarkedAsWrong(t *testing.T) {
	var out strings.Builder
	say := report.Narrate(&out)

	say.Checked(3)
	say.Note("this mode starts no session")
	say.Failed("camp up failed. Nothing of this composition is mounted.")

	text := out.String()
	for marker, want := range map[string]string{
		report.MarkOK:    "checked:",
		report.MarkNote:  "starts no session",
		report.MarkError: "camp up failed",
	} {
		var found bool
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, marker) && strings.Contains(line, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no line begins %s and carries %q:\n%s", marker, want, text)
		}
	}

	// And the prose starts in one column whichever marker precedes it, so
	// the text reads as one paragraph down the page rather than stepping
	// left and right with the width of the word in brackets.
	width := len(report.Marked(report.MarkOK, "x")) - 1
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if len(line) <= width || line[width] == ' ' {
			t.Errorf("the text of %q does not start in the shared column", line)
		}
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
