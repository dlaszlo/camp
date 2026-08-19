package fsx

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dlaszlo/camp/internal/pathx"
)

// The schedule these measure is two processes replacing one name at once.
//
// It has to be two processes and not two goroutines. What went wrong was
// that both writers opened one temporary name -- so both descriptors
// referred to one inode -- and a single process could be made to look
// correct by a mutex that a second camp does not share. The two here are
// two runs of this test binary, coordinated through files, so the only
// thing keeping them apart is what the package does.

const (
	writerEnv  = "CAMP_TEST_WRITER"
	syncEnv    = "CAMP_TEST_SYNC"
	rootEnv    = "CAMP_TEST_WRITER_ROOT"
	payloadEnv = "CAMP_TEST_PAYLOAD"
	// record is the name both writers replace, and hash the work area they
	// replace it in.
	record = "record.json"
	hash   = "cbfbbb63ee0d"
)

// TestAtomicWriteHelper is one of the two writers. It runs only when the
// parent below starts it, and it pauses between opening its temporary and
// writing anything into it, which is the moment the two used to share an
// inode.
func TestAtomicWriteHelper(t *testing.T) {
	label := os.Getenv(writerEnv)
	if label == "" {
		t.Skip("started only by TestTwoProcessesReplacingOneName")
	}
	meeting := os.Getenv(syncEnv)

	root, err := pathx.OpenRoot(os.Getenv(rootEnv))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	size, err := strconv.Atoi(os.Getenv(payloadEnv))
	if err != nil {
		t.Fatal(err)
	}

	afterTemporaryOpen = func() {
		announce(t, meeting, "opened-"+label, "")
		await(t, meeting, "go-"+label)
	}

	outcome := "ok"
	if err := Work(root, hash).Write(record, bytes.Repeat([]byte(label), size), 0o600); err != nil {
		outcome = err.Error()
	}
	announce(t, meeting, "done-"+label, outcome)
}

// Two writers, both holding an open temporary, and the first publishes
// before the second writes a byte.
//
// The failing sequence this is written against: both open .record.json.camp,
// A writes, syncs and renames it into place and returns success, and B
// then writes through its own descriptor -- which now refers to the
// published file. The bytes at the name were half of each payload, and
// they changed after a successful atomic write had returned.
func TestTwoProcessesReplacingOneName(t *testing.T) {
	env := t.TempDir()
	meeting := t.TempDir()

	root, err := pathx.OpenRoot(env)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	area := Work(root, hash)
	if err := area.Ensure(0o755); err != nil {
		t.Fatal(err)
	}
	published := filepath.Join(area.Root(), record)

	// Different sizes, so a payload that survived in part is visible as a
	// length as well as a pattern.
	first, second := 4096, 1024
	a := writer(t, "A", env, meeting, first)
	b := writer(t, "B", env, meeting, second)
	defer func() { a.Wait(); b.Wait() }()

	await(t, meeting, "opened-A")
	await(t, meeting, "opened-B")

	// A alone, all the way to success.
	announce(t, meeting, "go-A", "")
	await(t, meeting, "done-A")
	if outcome := said(t, meeting, "done-A"); outcome != "ok" {
		t.Fatalf("the first writer failed: %s", outcome)
	}
	wanted := bytes.Repeat([]byte("A"), first)
	if got := read(t, published); !bytes.Equal(got, wanted) {
		t.Fatalf("a successful write published %d bytes of %q, wanted %d of \"A\"",
			len(got), kinds(got), first)
	}

	// B now, through a descriptor it opened before A published.
	announce(t, meeting, "go-B", "")
	await(t, meeting, "done-B")
	outcome := said(t, meeting, "done-B")

	got := read(t, published)
	switch outcome {
	case "ok":
		// A writer that reported success owns the name, whole.
		if !bytes.Equal(got, bytes.Repeat([]byte("B"), second)) {
			t.Fatalf("the second writer succeeded and the name holds %d bytes of "+
				"%q, wanted %d of \"B\"", len(got), kinds(got), second)
		}
	default:
		// A writer that failed may not have touched what was there.
		if !bytes.Equal(got, wanted) {
			t.Fatalf("the second writer failed (%s) and still changed the "+
				"published file: %d bytes of %q", outcome, len(got), kinds(got))
		}
	}
}

// A failure at any step of the replacement leaves the name as it was, and
// leaves nothing else behind either: the temporary is this call's own, so
// cleaning it up cannot take another writer's with it.
func TestAFailedReplacementLeavesTheNameAndTheDirectoryAsTheyWere(t *testing.T) {
	env := t.TempDir()
	root, err := pathx.OpenRoot(env)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	area := Work(root, hash)
	if err := area.Ensure(0o755); err != nil {
		t.Fatal(err)
	}
	if err := area.Write(record, []byte("the first body"), 0o600); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("the injected failure")
	t.Cleanup(func() { failStep = func(string) error { return nil } })
	for _, step := range []string{"write", "chmod", "sync", "rename"} {
		failStep = func(at string) error {
			if at == step {
				return injected
			}
			return nil
		}
		if err := area.Write(record, []byte("the second body"), 0o600); !errors.Is(err, injected) {
			t.Fatalf("failing at %s returned %v", step, err)
		}
		if got := read(t, filepath.Join(area.Root(), record)); string(got) != "the first body" {
			t.Errorf("failing at %s changed the published file to %q", step, got)
		}
		if left := listing(t, area.Root()); len(left) != 1 || left[0] != record {
			t.Errorf("failing at %s left %v in the directory", step, left)
		}
	}

	failStep = func(string) error { return nil }
	if err := area.Write(record, []byte("the second body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(area.Root(), record)); string(got) != "the second body" {
		t.Errorf("the write that succeeded published %q", got)
	}
}

// A publication that fails anywhere leaves no name behind.
//
// This is what a report stands on. The final name used to be claimed
// first, as an empty file, and filled in afterwards: between the two it
// was an unseen report of nothing, which a camp command running at that
// moment printed and marked, and which a write that failed left there for
// good. The name and the bytes arrive together now, so every way the
// write can stop has to leave the name unclaimed.
func TestAFailedPublicationLeavesNoName(t *testing.T) {
	env := t.TempDir()
	root, err := pathx.OpenRoot(env)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	area := Work(root, hash)
	if err := area.Ensure(0o755); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("the injected failure")
	t.Cleanup(func() { failStep = func(string) error { return nil } })
	for _, step := range []string{"write", "chmod", "sync", "rename"} {
		failStep = func(at string) error {
			if at == step {
				return injected
			}
			return nil
		}
		if err := area.WriteNew(record, []byte("a whole body"), 0o600); !errors.Is(err, injected) {
			t.Fatalf("failing at %s returned %v", step, err)
		}
		if left := listing(t, area.Root()); len(left) != 0 {
			t.Errorf("failing at %s left %v in the directory", step, left)
		}
	}

	// And nothing that succeeded is lost: the same call, unobstructed,
	// publishes the whole body under the name it was refused before.
	failStep = func(string) error { return nil }
	if err := area.WriteNew(record, []byte("a whole body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(area.Root(), record)); string(got) != "a whole body" {
		t.Errorf("the publication that succeeded left %q", got)
	}
	// A second publication of the same name is refused rather than made to
	// replace it, so the caller can go and pick another name.
	if err := area.WriteNew(record, []byte("a second body"), 0o600); !errors.Is(err, ErrExists) {
		t.Fatalf("publishing over a name that is taken returned %v", err)
	}
	if got := read(t, filepath.Join(area.Root(), record)); string(got) != "a whole body" {
		t.Errorf("the refused publication changed the file to %q", got)
	}
}

// -- the two processes ------------------------------------------------------

func writer(t *testing.T, label, env, meeting string, size int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAtomicWriteHelper$", "-test.v")
	cmd.Env = append(os.Environ(),
		writerEnv+"="+label,
		syncEnv+"="+meeting,
		rootEnv+"="+env,
		payloadEnv+"="+strconv.Itoa(size))
	// Both streams to the parent's stderr, so a child that fails says so in
	// the run that started it.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

// announce and await are the whole coordination: a name appearing in a
// directory both processes can see. No pipe, because either side may be
// blocked inside a syscall this test is about and a reader that has to be
// serviced would add a schedule of its own.
// announce publishes one rendezvous name, and does it atomically.
//
// Written to a temporary and renamed into place, which is the same rule
// the code under test is about -- for the same reason, and this file is
// where forgetting it was measured. os.WriteFile creates the name first
// and fills it afterwards, so a reader that waits for the name to exist
// can read it while it is still empty. That is not a theoretical window:
// on a loaded machine the first writer's outcome came back as the empty
// string, and the test reported it as a failed write with no reason
// given.
func announce(t *testing.T, meeting, name, body string) {
	t.Helper()
	temporary := filepath.Join(meeting, "."+name+".partial")
	if err := os.WriteFile(temporary, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, filepath.Join(meeting, name)); err != nil {
		t.Fatal(err)
	}
}

func await(t *testing.T, meeting, name string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(meeting, name)); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s never appeared in %s", name, meeting)
}

func said(t *testing.T, meeting, name string) string {
	t.Helper()
	return string(read(t, filepath.Join(meeting, name)))
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func listing(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// kinds renders which payloads a block of bytes is made of, so a failure
// says "AB" rather than four kilobytes.
func kinds(data []byte) string {
	var seen []byte
	for _, b := range data {
		if !bytes.ContainsRune(seen, rune(b)) {
			seen = append(seen, b)
		}
	}
	return string(seen)
}
