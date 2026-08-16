package envx_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/envx"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/testenv"
)

const live = "/home/someone/dev/project-live"

// rule returns the identifier of the rule an expression was refused with.
func rule(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, and the value was accepted")
	}
	var single refusal.R
	if errors.As(err, &single) {
		return single.Rule
	}
	t.Fatalf("expected a refusal, got a plain error: %v", err)
	return ""
}

// resolve parses and resolves in one step, for the tests that are about
// the result rather than about where a problem was caught.
func resolve(t *testing.T, name, value string, environ ...string) (string, error) {
	t.Helper()
	expression, err := envx.Parse(name, value)
	if err != nil {
		return "", err
	}
	return expression.Resolve(envx.NewBase(environ, live))
}

// What the grammar accepts, and what each form produces.
func TestInterpolationProducesTheseBytes(t *testing.T) {
	base := []string{"HOME=/home/someone", "PATH=/usr/bin", "EMPTY=", "DOLLAR=$X"}

	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"a literal value passes through", "ssh -F /etc/none", "ssh -F /etc/none"},
		{"a bare reference", "$HOME/.ssh/config", "/home/someone/.ssh/config"},
		{"a braced reference", "${HOME}/.ssh/config", "/home/someone/.ssh/config"},
		{"the braced form separates a name from what follows",
			"${CAMP_LIVE}bin", live + "bin"},
		{"prepending, which is what the form exists for",
			"$CAMP_LIVE/.workspace/bin:$PATH", live + "/.workspace/bin:/usr/bin"},
		{"two references with nothing between them", "$HOME$HOME",
			"/home/someone/home/someone"},
		{"a dollar pair is one literal dollar", "$$HOME", "$HOME"},
		{"a dollar pair beside a reference", "$$$HOME", "$/home/someone"},
		{"an inherited name set to empty expands to empty", "[$EMPTY]", "[]"},
		{"inserted bytes are not scanned again", "$DOLLAR", "$X"},
		{"an empty value stays empty", "", ""},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolve(t, "NAME", test.value, base...)
			if err != nil {
				t.Fatalf("%q was refused: %v", test.value, err)
			}
			if got != test.want {
				t.Errorf("%q resolved to %q, wanted %q", test.value, got, test.want)
			}
		})
	}
}

// Declarations read the environment camp was started with, never each
// other. That is what makes the mapping's order meaningless and a cycle
// impossible to write down.
func TestADeclarationNeverReadsASibling(t *testing.T) {
	base := envx.NewBase([]string{"B=inherited"}, live)

	a, err := envx.Parse("A", "$B")
	if err != nil {
		t.Fatal(err)
	}
	// B is declared here as something else entirely; A must not see it.
	if _, err := envx.Parse("B", "declared"); err != nil {
		t.Fatal(err)
	}
	got, err := a.Resolve(base)
	if err != nil {
		t.Fatal(err)
	}
	if got != "inherited" {
		t.Errorf("A resolved to %q; a sibling declaration is not an interpolation "+
			"input, so it should have read the inherited B", got)
	}
}

// Every refusal in the grammar, with the rule that has to fire. The rules
// are what the tests and the messages both hang on, so each one is named
// here rather than checked by the text of a sentence.
func TestTheGrammarsRefusals(t *testing.T) {
	cases := []struct {
		name  string
		value string
		rule  string
		says  string
	}{
		{"a lone dollar", "cost: 5$", "environment-expansion", "$$"},
		{"a dollar before a digit", "$1PATH", "environment-expansion", "$$"},
		{"an unclosed brace", "${HOME/.ssh", "environment-expansion", "never closed"},
		{"an empty braced name", "${}", "environment-expansion", "is not a name"},
		{"a braced name the bare form would not take", "${A-B}",
			"environment-expansion", "is not a name"},
		{"a NUL in a value", "before\x00after", "environment-value", "NUL"},
		{"a reference to PWD", "$PWD/sub", "environment-pwd", "$CAMP_LIVE"},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := envx.Parse("NAME", test.value)
			if got := rule(t, err); got != test.rule {
				t.Fatalf("%q fired %q, wanted %q", test.value, got, test.rule)
			}
			if !strings.Contains(err.Error(), test.says) {
				t.Errorf("the refusal does not mention %q:\n%v", test.says, err)
			}
		})
	}
}

// A malformed expression says where it is, so a long value does not have
// to be read character by character.
func TestAMalformedExpressionNamesTheByte(t *testing.T) {
	_, err := envx.Parse("PATH", "/usr/bin:$")
	if got := rule(t, err); got != "environment-expansion" {
		t.Fatalf("wanted environment-expansion, got %q", got)
	}
	if !strings.Contains(err.Error(), "byte 9") {
		t.Errorf("the refusal should name the offset of the dollar:\n%v", err)
	}
}

// An absent name is not an empty value. Substituting nothing for a typo is
// exactly the failure -- a setting that looks applied and is not -- that
// this whole section exists to avoid.
func TestAnAbsentNameIsRefusedAndAnEmptyOneIsNot(t *testing.T) {
	_, err := resolve(t, "TOOL", "$MISSING/bin", "SET=")
	if got := rule(t, err); got != "environment-undefined" {
		t.Fatalf("wanted environment-undefined, got %q", got)
	}
	if !strings.Contains(err.Error(), "not the same as") {
		t.Errorf("the refusal should distinguish absent from set-empty:\n%v", err)
	}

	got, err := resolve(t, "TOOL", "[$SET]", "SET=")
	if err != nil {
		t.Fatalf("a name set to the empty string is defined, and was refused: %v", err)
	}
	if got != "[]" {
		t.Errorf("resolved to %q, wanted %q", got, "[]")
	}
}

// camp's own names: only CAMP_LIVE is an input, and it is camp's value for
// it -- not whatever an outer session left in the environment.
func TestCampsOwnNamesAreNotInheritedInputs(t *testing.T) {
	outer := []string{"CAMP_LIVE=/somewhere/else", "CAMP_OTHER=x"}

	got, err := resolve(t, "TOOL", "$CAMP_LIVE/bin", outer...)
	if err != nil {
		t.Fatalf("$CAMP_LIVE was refused: %v", err)
	}
	if got != live+"/bin" {
		t.Errorf("resolved to %q; a session entering a composition must see that "+
			"composition, not the one it was started from", got)
	}

	_, err = resolve(t, "TOOL", "$CAMP_OTHER", outer...)
	if got := rule(t, err); got != "environment-undefined" {
		t.Fatalf("an inherited CAMP_ name fired %q, wanted environment-undefined", got)
	}
}

// The names a configuration may not declare, and the ones it may not form.
func TestNamesThatCannotBeDeclared(t *testing.T) {
	cases := []struct {
		name string
		rule string
		says string
	}{
		{"", "environment-name", "empty name"},
		{"A=B", "environment-name", "'='"},
		{"A\x00B", "environment-name", "NUL"},
		{"PWD", "environment-reserved", "working directory"},
		{"CAMP_LIVE", "environment-reserved", "camp's"},
		{"CAMP_ANYTHING", "environment-reserved", "camp's"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := envx.CheckName(test.name)
			if got := rule(t, err); got != test.rule {
				t.Fatalf("%q fired %q, wanted %q", test.name, got, test.rule)
			}
			if !strings.Contains(err.Error(), test.says) {
				t.Errorf("the refusal does not mention %q:\n%v", test.says, err)
			}
		})
	}

	for _, name := range []string{"PATH", "GIT_SSH_COMMAND", "_x", "CAMP", "CAMPY"} {
		if err := envx.CheckName(name); err != nil {
			t.Errorf("%q is a legal name and was refused: %v", name, err)
		}
	}
}

// What a report may show. Literal text is already in the file being
// described; an inherited value is not, and must not be copied into a
// transcript because somebody asked what would mount.
func TestDisplayShowsLiteralsAndNamesInheritedInsertions(t *testing.T) {
	cases := []struct {
		value string
		want  string
	}{
		{"ssh -F $HOME/.ssh/config", `"ssh -F " + <inherited HOME> + "/.ssh/config"`},
		{"$CAMP_LIVE/.workspace/bin:$PATH",
			`"` + live + `/.workspace/bin:" + <inherited PATH>`},
		{"plain", `"plain"`},
		{"", `""`},
		{"$PATH", `<inherited PATH>`},
	}
	for _, test := range cases {
		expression, err := envx.Parse("NAME", test.value)
		if err != nil {
			t.Fatalf("%q was refused: %v", test.value, err)
		}
		if got := expression.Display(live); got != test.want {
			t.Errorf("%q displayed as %s, wanted %s", test.value, got, test.want)
		}
	}
}

// A control byte in a value is shown reversibly rather than sent to the
// terminal, which would let a value rewrite the report around it.
func TestDisplayEscapesControlBytesReversibly(t *testing.T) {
	expression, err := envx.Parse("NAME", "a\tb\nc\x1b[2J")
	if err != nil {
		t.Fatal(err)
	}
	shown := expression.Display(live)
	for _, raw := range []string{"\t", "\n", "\x1b"} {
		if strings.Contains(shown, raw) {
			t.Errorf("a raw control byte reached the report: %q", shown)
		}
	}
	if shown != `"a\tb\nc\x1b[2J"` {
		t.Errorf("displayed as %s", shown)
	}
}

// The effective environment: one list, no name twice, nothing reordered
// for camp's convenience.
func TestTheEffectiveEnvironmentIsOneDuplicateFreeList(t *testing.T) {
	inherited := []string{
		"SHELL=/bin/bash",
		"PATH=/usr/bin",
		"HOME=/home/someone",
		"PATH=/ignored", // a second entry for a name the first already carried
		"CAMP_LIVE=/somewhere/else",
		"PWD=/where/camp/was/run",
		"malformed",
	}
	declared := []envx.Setting{
		{Name: "PATH", Value: live + "/bin:/usr/bin"},
		{Name: "ZZZ", Value: "last"},
		{Name: "AAA", Value: "first"},
	}
	owned := []envx.Setting{{Name: "CAMP_LIVE", Value: live}, {Name: "PWD", Value: live}}

	got := envx.Effective(inherited, declared, owned)
	want := []string{
		"SHELL=/bin/bash",
		"PATH=" + live + "/bin:/usr/bin",
		"HOME=/home/someone",
		"AAA=first",
		"ZZZ=last",
		"CAMP_LIVE=" + live,
		"PWD=" + live,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the effective environment came out as\n%s\n\nwanted\n%s",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	seen := map[string]bool{}
	for _, entry := range got {
		name, _, _ := strings.Cut(entry, "=")
		if seen[name] {
			t.Errorf("%q appears twice in the child's environment", name)
		}
		seen[name] = true
	}
}

// New names arrive in byte order, so two orderings of the same map produce
// the same list -- and therefore the same session.
func TestNewNamesAreAppendedInByteOrder(t *testing.T) {
	one := envx.Effective(nil, []envx.Setting{
		{Name: "B", Value: "2"}, {Name: "a", Value: "3"}, {Name: "A", Value: "1"},
	}, nil)
	other := envx.Effective(nil, []envx.Setting{
		{Name: "a", Value: "3"}, {Name: "A", Value: "1"}, {Name: "B", Value: "2"},
	}, nil)
	if strings.Join(one, ",") != strings.Join(other, ",") {
		t.Errorf("two orderings produced %v and %v", one, other)
	}
	if strings.Join(one, ",") != "A=1,B=2,a=3" {
		t.Errorf("byte order came out as %v", one)
	}
}

// The lookup rule that keeps a declared PATH honest: the workload's own
// PATH selects the command, and camp's never does.
func TestABareCommandIsFoundThroughTheGivenPath(t *testing.T) {
	root := testenv.Root(t)
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for _, directory := range []string{first, second} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string) {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(first, "tool"))
	write(filepath.Join(second, "tool"))
	write(filepath.Join(second, "only-here"))
	if err := os.WriteFile(filepath.Join(first, "data"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	path := first + ":" + second
	cases := []struct {
		argv0 string
		want  string
	}{
		{"tool", filepath.Join(first, "tool")},
		{"only-here", filepath.Join(second, "only-here")},
		{"/bin/sh", "/bin/sh"},
		{"./tool", "./tool"},
	}
	for _, test := range cases {
		got, err := envx.Command(test.argv0, path)
		if err != nil {
			t.Fatalf("%q was not resolved: %v", test.argv0, err)
		}
		if got != test.want {
			t.Errorf("%q resolved to %q, wanted %q", test.argv0, got, test.want)
		}
	}

	// A path that names no such command, and a name that exists but cannot
	// be executed, both fail the same way -- and neither prints the path.
	for _, argv0 := range []string{"absent", "data"} {
		_, err := envx.Command(argv0, path)
		if got := rule(t, err); got != "workload-not-found" {
			t.Fatalf("%q fired %q", argv0, got)
		}
		if strings.Contains(err.Error(), first) {
			t.Errorf("the failure printed the PATH it searched:\n%v", err)
		}
	}
}
