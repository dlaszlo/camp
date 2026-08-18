package refusal_test

import (
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/refusal"
)

// The group used throughout: one problem, three parts, and a subject
// supplied wherever the rule fires.
var missing = refusal.Group{
	Rule:   "target-missing",
	One:    "a mount point does not exist:",
	Many:   "%d mount points do not exist:",
	Detail: "A bind mount cannot create its own mount point.",
}

// Nine mounts failing one check are one problem with nine paths, not nine
// problems. The explanation appears once, and every path appears.
func TestOneCheckFiringManyTimesIsOneRefusal(t *testing.T) {
	var list refusal.List
	for _, path := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		list.Group(missing, "%q", path)
	}

	if list.Count() != 1 {
		t.Fatalf("nine subjects of one rule count as %d problems", list.Count())
	}
	merged := list.Merge()
	if len(merged) != 1 {
		t.Fatalf("nine subjects merged into %d refusals", len(merged))
	}
	text := merged[0].Message
	if got := strings.Count(text, "A bind mount cannot"); got != 1 {
		t.Errorf("the explanation appears %d times:\n%s", got, text)
	}
	if !strings.Contains(text, "9 mount points do not exist") {
		t.Errorf("the opening does not count the subjects:\n%s", text)
	}
	for _, path := range []string{`"a"`, `"e"`, `"i"`} {
		if !strings.Contains(text, path) {
			t.Errorf("%s is not named in the refusal:\n%s", path, text)
		}
	}
	if merged[0].Rule != "target-missing" {
		t.Errorf("the merged refusal lost its rule: %q", merged[0].Rule)
	}
}

// One subject reads as one subject: the singular opening, and no count.
func TestOneSubjectReadsAsOne(t *testing.T) {
	var list refusal.List
	list.Group(missing, "%q", "a")

	text := list.Merge()[0].Message
	if !strings.HasPrefix(text, "a mount point does not exist:") {
		t.Errorf("the singular opening was not used:\n%s", text)
	}
	if strings.Contains(text, "1 mount point") {
		t.Errorf("one subject was counted at the reader:\n%s", text)
	}
	if list.Error() != text {
		t.Errorf("the error text and the message differ:\n%q\n%q", list.Error(), text)
	}
}

// Two rules, or one rule with two explanations, are two problems. The
// explanation is what says whether the reader's next move is the same, so
// refusals whose explanations differ are never merged.
func TestDifferentProblemsStayApart(t *testing.T) {
	other := missing
	other.Detail = "Something else entirely."

	var list refusal.List
	list.Group(missing, "%q", "a")
	list.Group(other, "%q", "b")
	list.Add("overlap", "a name exists in both repositories")

	if list.Count() != 3 {
		t.Fatalf("three problems counted as %d", list.Count())
	}
	// A refusal that belongs to no group passes through untouched, which is
	// every rule that can only ever fire about one thing.
	if last := list.Merge()[2]; last.Message != "a name exists in both repositories" {
		t.Errorf("an ungrouped refusal was rewritten: %q", last.Message)
	}
}

// The order is the order the checks ran in, so a reader meets the
// problems where they happened rather than where the merging put them.
func TestOrderIsTheOrderTheChecksRan(t *testing.T) {
	var list refusal.List
	list.Add("first", "one")
	list.Group(missing, "%q", "a")
	list.Add("second", "two")
	list.Group(missing, "%q", "b")

	rules := list.Merge().Rules()
	want := []string{"first", "target-missing", "second"}
	if strings.Join(rules, ",") != strings.Join(want, ",") {
		t.Errorf("the merged order is %v and the checks ran %v", rules, want)
	}
}

// Merging twice changes nothing: report rendering and the error path both
// merge, and one of them may already have.
func TestMergingIsIdempotent(t *testing.T) {
	var list refusal.List
	list.Group(missing, "%q", "a")
	list.Group(missing, "%q", "b")

	once := list.Merge()
	twice := once.Merge()
	if len(twice) != 1 || twice[0].Message != once[0].Message {
		t.Errorf("merging a merged list changed it:\n%q\n%q",
			once[0].Message, twice[0].Message)
	}
}
