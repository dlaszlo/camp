package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/mountx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/report"
	"github.com/dlaszlo/camp/internal/state"
	"github.com/dlaszlo/camp/internal/testenv"
)

// composition is a privileged composition's record and the plan it was
// written from.
func composition(t *testing.T) (state.Record, plan.Mount) {
	t.Helper()
	env := testenv.NewEnv(t)
	cfg := env.Config(t, "")
	built, refused := plan.Prepare(cfg, plan.Privileged)
	if !refused.Empty() {
		t.Fatalf("the fixture was refused:\n%v", refused)
	}
	record := state.FromPlan(built, filepath.Join(built.Work, "staging"),
		"test", "cfgdigest", "invdigest", os.Getuid(), os.Getgid())
	// The phase a verified composition is in, so that what these tests
	// read is the comparison and not the sentence a half-finished run
	// leaves behind.
	record.Phase = state.Up
	for _, mount := range built.Mounts {
		if mount.Kind == plan.Overlay {
			return record, mount
		}
	}
	t.Fatal("the fixture plans no composed tree")
	return state.Record{}, plan.Mount{}
}

// standingAs is what the kernel's table would say about the composed
// tree if it were mounted from a description.
func standingAs(described mountx.OverlayConfig, point string) []mountinfo.Entry {
	entry := mountinfo.Entry{
		Point:  point,
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
	return []mountinfo.Entry{entry}
}

func saying(t *testing.T, record state.Record, table []mountinfo.Entry) (string, error) {
	t.Helper()
	var out, said bytes.Buffer
	ctx := &context{out: &out, err: report.To(&said)}
	err := describeRecord(ctx, record, table)
	ctx.err.Close()
	return out.String() + said.String(), err
}

// 'camp status' reads the recorded operands back and holds the standing
// composed tree to them.
//
// This is what the record's operands are for, and the reason they are
// kept at all. Identity answers whether the object at a path is the one
// camp mounted -- an overlay's device and inode prove that much and say
// nothing about which layers are underneath it. So a composed tree
// standing over another lower, or without the extended-attribute
// namespace it was made with, passed every check camp had while the
// record described a mount nobody made.
func TestStatusHoldsTheComposedTreeToWhatTheRecordSays(t *testing.T) {
	record, planned := composition(t)
	described := mountx.DescribeOverlay(planned)

	t.Run("as recorded", func(t *testing.T) {
		said, err := saying(t, record, standingAs(described, planned.Target))
		if err != nil {
			t.Errorf("a composed tree made of exactly what the record says was "+
				"reported as wrong: %v\n%s", err, said)
		}
		if strings.Contains(said, "not as recorded") {
			t.Errorf("a composed tree made of exactly what the record says "+
				"disagrees with it:\n%s", said)
		}
		// And what it is made of is said, every time and not only when
		// something is wrong: it is the one line that says what the tree
		// somebody is standing in was composed from.
		if !strings.Contains(said, "overlay, made with lowerdir=") {
			t.Errorf("status does not say what the composed tree is made "+
				"of:\n%s", said)
		}
	})

	for _, probe := range []struct {
		name  string
		spoil func(*mountinfo.Entry)
		says  string
	}{
		{"another lower", func(e *mountinfo.Entry) {
			e.Super["lowerdir+"] = "/somebody/elses/workspace"
		}, "lowerdir"},
		{"another upper", func(e *mountinfo.Entry) {
			e.Super["upperdir"] = "/somebody/elses/code"
		}, "upperdir"},
		{"the other extended-attribute namespace", func(e *mountinfo.Entry) {
			delete(e.Super, planned.Xattr)
			e.Super["userxattr"] = ""
		}, planned.Xattr},
		{"not an overlay at all", func(e *mountinfo.Entry) {
			e.FSType = "tmpfs"
		}, "tmpfs"},
	} {
		t.Run(probe.name, func(t *testing.T) {
			table := standingAs(described, planned.Target)
			probe.spoil(&table[0])
			said, err := saying(t, record, table)
			if err == nil {
				t.Errorf("status reported a clean composition over %s:\n%s",
					probe.name, said)
			}
			if !strings.Contains(said, "not as recorded") {
				t.Fatalf("status did not say the composed tree is not what the "+
					"record describes:\n%s", said)
			}
			if !strings.Contains(said, probe.says) {
				t.Errorf("status did not name what differs (%s):\n%s", probe.says, said)
			}
		})
	}
}

// A record written before the operands were kept is not a disagreement.
//
// It answers every question a teardown asks, which is what it answered
// when it was written. A comparison with nothing to compare against is
// not a mismatch, and reporting one would make every older composition
// look wrong.
func TestARecordWithNoRecordedOperandsIsNotADisagreement(t *testing.T) {
	record, planned := composition(t)
	described := mountx.DescribeOverlay(planned)
	for index := range record.Mounts {
		record.Mounts[index].Operands = nil
		record.Mounts[index].FSType = ""
		record.Mounts[index].Options = ""
	}

	said, err := saying(t, record, standingAs(described, planned.Target))
	if err != nil {
		t.Errorf("a record from before the operands were kept was reported as "+
			"wrong: %v\n%s", err, said)
	}
	if strings.Contains(said, "not as recorded") {
		t.Errorf("a record with nothing to compare produced a "+
			"disagreement:\n%s", said)
	}
}
