package mountx

import (
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/plan"
)

// composed is an overlay of the shape camp plans: two lower layers in a
// deliberate order, a writable upper with its work directory, and the
// extended-attribute namespace the mode decides.
func composed() plan.Mount {
	return plan.Mount{
		Kind:   plan.Overlay,
		Role:   plan.Composed,
		Target: "/env/live",
		Lower:  []string{"/env/workspace", "/env/other"},
		Upper:  "/env/code",
		Work:   "/env/work/overlay",
		Xattr:  "userxattr",
	}
}

// The description says what the plan asks for, in the order the kernel
// is given it.
//
// This is the one place the operands are derived, so it is the one place
// they can be got wrong without anything else disagreeing. Everything
// below holds the other readers to this description; this holds the
// description to the plan.
func TestTheDescriptionIsWhatThePlanAsksFor(t *testing.T) {
	described := DescribeOverlay(composed())

	if described.FSType != "overlay" {
		t.Errorf("the composed tree would be asked of fsopen as %q", described.FSType)
	}
	want := []Fsconfig{
		{Key: "lowerdir+", Path: "/env/workspace", Operand: OperandLower, Index: 0},
		{Key: "lowerdir+", Path: "/env/other", Operand: OperandLower, Index: 1},
		{Key: "upperdir", Path: "/env/code", Operand: OperandUpper},
		{Key: "workdir", Path: "/env/work/overlay", Operand: OperandWork},
		{Key: "userxattr"},
	}
	if len(described.Steps) != len(want) {
		t.Fatalf("the overlay is described in %d calls and the plan asks for "+
			"%d:\n%v", len(described.Steps), len(want), described.Steps)
	}
	for index, step := range described.Steps {
		if step != want[index] {
			t.Errorf("call %d is %+v and the plan asks for %+v", index+1, step, want[index])
		}
	}

	// A read-only overlay has no upper, and therefore no work directory:
	// the pair is one decision.
	readOnly := composed()
	readOnly.Upper, readOnly.Work = "", ""
	for _, step := range DescribeOverlay(readOnly).Steps {
		if step.Operand == OperandUpper || step.Operand == OperandWork {
			t.Errorf("an overlay with no upper is still described as having %s", step.Key)
		}
	}
}

// What camp sends the kernel is the description, call for call.
//
// The mutation this exists for: change an fsconfig key, drop a flag, or
// swap the lower layers in the sequence that performs the mount, and
// leave the description alone. Before there was a description that was a
// record and a verification describing a mount nobody made, with every
// test green. Now it is this test failing.
func TestTheKernelIsSentExactlyWhatIsDescribed(t *testing.T) {
	type call struct {
		key  string
		fd   int
		flag bool
	}
	var sent []call
	original, originalFlag := fsconfigFd, fsconfigFlag
	fsconfigFd = func(_ int, key string, fd int) error {
		sent = append(sent, call{key: key, fd: fd})
		return nil
	}
	fsconfigFlag = func(_ int, key string) error {
		sent = append(sent, call{key: key, flag: true})
		return nil
	}
	t.Cleanup(func() { fsconfigFd, fsconfigFlag = original, originalFlag })

	// Descriptors nothing will use: the calls that would use them are
	// replaced above, and what is being measured is which number reaches
	// which key.
	ends := Operands{Lower: []int{11, 12}, Upper: 13, Work: 14}
	described := DescribeOverlay(composed())
	// The context is never touched either, for the same reason: a real one
	// comes from fsopen, and nothing in this repository may mount.
	if err := fill(-1, described, ends); err != nil {
		t.Fatalf("filling the filesystem context: %v", err)
	}

	if len(sent) != len(described.Steps) {
		t.Fatalf("%d calls were made and the overlay is described in %d:\n%v",
			len(sent), len(described.Steps), sent)
	}
	for index, step := range described.Steps {
		made := sent[index]
		if made.key != step.Key {
			t.Errorf("call %d gave the kernel %q and the description says %q",
				index+1, made.key, step.Key)
		}
		if made.flag != step.Flag() {
			t.Errorf("call %d is a flag=%v and the description says flag=%v",
				index+1, made.flag, step.Flag())
		}
		if step.Flag() {
			continue
		}
		fd, err := ends.For(step)
		if err != nil {
			t.Fatalf("the description names an operand that was not opened: %v", err)
		}
		if made.fd != fd {
			t.Errorf("call %d gave the kernel descriptor %d for %s, and %s was "+
				"opened as %d", index+1, made.fd, step.Key, step.Path, fd)
		}
	}
}

// An operand the description names and nobody opened is refused, and
// never answered with a descriptor picked out of the zero value.
//
// Descriptor 0 is this process's standard input. Giving the kernel that
// as a lower layer is a composed tree made of something nobody named.
func TestAnOperandThatWasNeverOpenedIsRefused(t *testing.T) {
	_, err := NoOperands().For(Fsconfig{
		Key: "lowerdir+", Path: "/env/workspace", Operand: OperandLower})
	if err == nil {
		t.Fatal("a lower layer that was never opened answered with a descriptor")
	}
	if !strings.Contains(err.Error(), "/env/workspace") {
		t.Errorf("the refusal does not name the operand: %v", err)
	}
}

// The option string a person reads is a rendering of the same calls.
func TestTheOptionLineRendersTheSameCalls(t *testing.T) {
	described := DescribeOverlay(composed())
	options := described.Options()

	if options != "lowerdir=/env/workspace:/env/other,upperdir=/env/code,"+
		"workdir=/env/work/overlay,userxattr" {
		t.Errorf("the option line is %q", options)
	}
	// Every operand the kernel is given appears in it, and in the order it
	// is given: the line is read by somebody deciding whether the tree
	// standing in front of them is the one the record describes.
	for _, step := range described.Steps {
		if step.Flag() {
			continue
		}
		if !strings.Contains(options, Escape(step.Path)) {
			t.Errorf("the option line does not carry %s: %q", step.Path, options)
		}
	}
}

// The comparison holds a mounted overlay to the description, operand by
// operand, and notices an order it was not given in.
func TestTheComparisonNoticesEveryOneSidedChange(t *testing.T) {
	described := DescribeOverlay(composed())
	mounted := asMounted(described)

	if wrong := described.Mismatches(mounted); len(wrong) != 0 {
		t.Fatalf("a mount made of exactly what was described disagrees with "+
			"it: %+v", wrong)
	}

	for _, probe := range []struct {
		name  string
		spoil func(mountinfo.Entry)
		says  string
	}{
		{"another lower", func(e mountinfo.Entry) {
			e.Super["lowerdir+"] = "/env/somebody-else"
		}, "lowerdir"},
		{"the lowers the other way round", func(e mountinfo.Entry) {
			e.Super["lowerdir+"] = "/env/other:/env/workspace"
		}, "lowerdir"},
		{"another upper", func(e mountinfo.Entry) {
			e.Super["upperdir"] = "/env/somebody-else"
		}, "upperdir"},
		{"no work directory", func(e mountinfo.Entry) {
			delete(e.Super, "workdir")
		}, "workdir"},
		{"the other extended-attribute namespace", func(e mountinfo.Entry) {
			delete(e.Super, "userxattr")
			e.Super["nouserxattr"] = ""
		}, "userxattr"},
	} {
		t.Run(probe.name, func(t *testing.T) {
			entry := asMounted(described)
			probe.spoil(entry)
			wrong := described.Mismatches(entry)
			if len(wrong) == 0 {
				t.Fatalf("%s was not noticed", probe.name)
			}
			if wrong[0].Key != probe.says {
				t.Errorf("the disagreement is about %q and %q was changed",
					wrong[0].Key, probe.says)
			}
		})
	}
}

// asMounted is what the kernel's table would say about a mount made from
// a description: the operands it was given, plus defaults of its own that
// the comparison has to ignore.
func asMounted(described OverlayConfig) mountinfo.Entry {
	entry := mountinfo.Entry{
		Point:  "/env/live",
		FSType: described.FSType,
		Super:  map[string]string{"redirect_dir": "on", "uuid": "null"},
	}
	for _, step := range described.Steps {
		if step.Flag() {
			entry.Super[step.Key] = ""
			continue
		}
		if held, found := entry.Super[step.Key]; found {
			entry.Super[step.Key] = held + ":" + step.Path
			continue
		}
		entry.Super[step.Key] = step.Path
	}
	return entry
}

// The description and the table entry a test builds from it agree about
// the two spellings of a lower layer, which is what the comparison reads.
func TestBothSpellingsOfALowerLayerAreRead(t *testing.T) {
	described := DescribeOverlay(composed())
	// The old option-string form, as a mount made by hand or by an older
	// camp reports it.
	old := asMounted(described)
	old.Super["lowerdir"] = old.Super["lowerdir+"]
	delete(old.Super, "lowerdir+")

	if wrong := described.Mismatches(old); len(wrong) != 0 {
		t.Errorf("a mount reporting its lowers under the other spelling reads "+
			"as a disagreement: %+v", wrong)
	}
}
