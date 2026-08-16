package enc_test

import (
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/enc"
)

// The names camp has to be able to write down truthfully.
//
// Every one of these is a legal Linux filename, and the earlier record
// format -- fields separated by a space and an arrow -- could not
// represent several of them: two different filesystem states serialised
// to the same line, or produced a line that would not parse back. A
// snapshot that cannot represent what it saw will one day report a change
// that did not happen, or miss one that did.
var hostile = []struct {
	name    string
	value   string
	encoded string
}{
	{"ordinary", "CLAUDE.md", "CLAUDE.md"},
	{"a space", "my notes.md", "my notes.md"},
	{"a tab", "col\tumn", `col\tumn`},
	{"a newline", "two\nlines", `two\nlines`},
	{"a carriage return", "over\rwritten", `over\rwritten`},
	{"a backslash", `back\slash`, `back\\slash`},
	{"the arrow the old format used", "a -> b", "a -> b"},
	{"an escape that looks like ours", `\t`, `\\t`},
	{"a control byte", "bell\x07", `bell\x07`},
	{"delete", "del\x7f", `del\x7f`},
	{"invalid UTF-8", "caf\xe9", "caf\xe9"},
	{"a leading dash", "-rf", "-rf"},
	{"empty", "", ""},
}

func TestEveryHostileNameSurvivesTheRoundTrip(t *testing.T) {
	for _, test := range hostile {
		t.Run(test.name, func(t *testing.T) {
			got := enc.Encode(test.value)
			if got != test.encoded {
				t.Errorf("encoded to %q, wanted %q", got, test.encoded)
			}
			back, err := enc.Decode(got)
			if err != nil {
				t.Fatalf("decoding %q: %v", got, err)
			}
			if back != test.value {
				t.Errorf("came back as %q, wanted %q", back, test.value)
			}
		})
	}
}

// The framing depends on this: an encoded field can never hold a raw tab
// or newline, so a tab can separate fields and a newline can separate
// records without either ever being ambiguous.
func TestAnEncodedFieldNeverHoldsAFramingByte(t *testing.T) {
	for _, test := range hostile {
		encoded := enc.Encode(test.value)
		if strings.ContainsAny(encoded, "\t\n\r") {
			t.Errorf("%q encoded to %q, which still holds a framing byte",
				test.value, encoded)
		}
	}
}

func TestFieldsAreSplitAndDecodedTogether(t *testing.T) {
	line := enc.Line("lower", "directory", "two\nlines")
	if strings.Count(line, "\t") != 2 {
		t.Fatalf("the record has the wrong shape: %q", line)
	}
	fields, err := enc.Fields(line)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || fields[2] != "two\nlines" {
		t.Errorf("the fields came back as %q", fields)
	}
}

// A line that does not decode refuses the whole file. Half-reading a
// snapshot is worse than not reading it.
func TestABadEscapeRefusesTheWholeFile(t *testing.T) {
	for _, bad := range []string{`ends with\`, `bad\q`, `short\x4`, `\xzz`} {
		if _, err := enc.Decode(bad); err == nil {
			t.Errorf("%q decoded and should not have", bad)
		}
	}
	if _, err := enc.Parse([]byte("good\tline\nbad\\qline\n")); err == nil {
		t.Error("a document with one undecodable line was accepted")
	}
}

// Sorting compares decoded bytes. A locale sort and a byte sort silently
// disagree, and the gate, the inventory and the exclude all have to
// describe the same set.
func TestSortingIsByDecodedBytes(t *testing.T) {
	lines := []string{
		enc.Line("b"),
		enc.Line("A"),
		enc.Line("a"),
		enc.Line("_"),
		enc.Line("Z"),
	}
	enc.Sort(lines)

	want := []string{"A", "Z", "_", "a", "b"}
	for index, line := range lines {
		fields, err := enc.Fields(line)
		if err != nil {
			t.Fatal(err)
		}
		if fields[0] != want[index] {
			t.Fatalf("entry %d is %q, wanted %q", index, fields[0], want[index])
		}
	}
}

func TestADocumentWithNoRecordsIsEmpty(t *testing.T) {
	if data := enc.Document(nil); len(data) != 0 {
		t.Errorf("an empty document came out as %q, and should be empty rather "+
			"than one blank line", data)
	}
	records, err := enc.Parse(nil)
	if err != nil || len(records) != 0 {
		t.Errorf("parsing an empty document gave %v, %v", records, err)
	}
}

// A symlink's target may legally contain a newline. The name may not --
// camp refuses that at up, because it cannot be written as a gitignore
// pattern -- but the snapshot layer has to be able to speak truthfully
// about everything else.
func TestASymlinkTargetWithANewlineIsRepresentable(t *testing.T) {
	line := enc.Line("lower", "symlink", "link", "/some/where\nelse")
	records, err := enc.Parse(enc.Document([]string{line}))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0][3] != "/some/where\nelse" {
		t.Errorf("the target came back as %q", records)
	}
}
