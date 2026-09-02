package logs_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dlaszlo/camp/internal/logs"
	"github.com/dlaszlo/camp/internal/pathx"
)

// Every line is kept, and every line says when it was said.
func TestEveryLineIsKeptWithItsTime(t *testing.T) {
	log, env := open(t)

	if _, err := log.Write([]byte("[OK] locks: taken\n[NOTE] only your own id is mapped\n")); err != nil {
		t.Fatalf("writing the log: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("closing the log: %v", err)
	}

	lines := read(t, logs.Path(env))
	if len(lines) != 2 {
		t.Fatalf("two lines were written and %d were kept:\n%s", len(lines), lines)
	}
	for _, line := range lines {
		stamp, text, found := strings.Cut(line, " ")
		if !found {
			t.Fatalf("a line carries no timestamp: %q", line)
		}
		if _, err := time.Parse(logs.Stamp, stamp); err != nil {
			t.Errorf("%q does not parse as the stamp camp writes: %v", stamp, err)
		}
		if !strings.HasPrefix(text, "[") {
			t.Errorf("the line lost its marker: %q", line)
		}
	}
	if !strings.Contains(lines[0], "locks: taken") || !strings.Contains(lines[1], "own id is mapped") {
		t.Errorf("the lines are not in the order they were said:\n%s", strings.Join(lines, "\n"))
	}
}

// A blank line stays blank. Refusals are paragraphs, and a timestamp on
// an empty line is a line that says nothing.
func TestABlankLineStaysBlank(t *testing.T) {
	log, env := open(t)
	log.Write([]byte("first\n\nsecond\n"))
	log.Close()

	lines := read(t, logs.Path(env))
	if len(lines) != 3 || lines[1] != "" {
		t.Errorf("the paragraph break was not kept:\n%q", lines)
	}
}

// The file rotates before it passes the limit, a few files are kept, and
// the oldest goes. Nothing here waits for a run to be over: a log that
// only rotated at exit would grow without bound during a long session.
func TestTheFileRotatesBySize(t *testing.T) {
	log, env := open(t)
	line := strings.Repeat("x", 4096) + "\n"
	for written := 0; written < logs.Limit*(logs.Kept+2); written += len(line) {
		if _, err := log.Write([]byte(line)); err != nil {
			t.Fatalf("writing the log: %v", err)
		}
	}
	log.Close()

	current := logs.Path(env)
	info, err := os.Stat(current)
	if err != nil {
		t.Fatalf("the current log is not there: %v", err)
	}
	if info.Size() > logs.Limit {
		t.Errorf("the current log is %d bytes and the limit is %d", info.Size(), logs.Limit)
	}
	for number := 1; number <= logs.Kept; number++ {
		if _, err := os.Stat(fmt.Sprintf("%s.%d", current, number)); err != nil {
			t.Errorf("the rotated file %d is not there: %v", number, err)
		}
	}
	if _, err := os.Stat(fmt.Sprintf("%s.%d", current, logs.Kept+1)); err == nil {
		t.Errorf("more than %d rotated files are kept", logs.Kept)
	}
}

// Two processes write one log -- a session's launcher and the init it
// re-executes -- and the one that did not rotate must not keep writing
// into the file that was rotated away.
func TestAWriterFollowsSomebodyElsesRotation(t *testing.T) {
	first, env := open(t)
	second, err := logs.Open(env, nil)
	if err != nil {
		t.Fatalf("opening the log a second time: %v", err)
	}
	defer second.Close()

	line := strings.Repeat("y", 4096) + "\n"
	for written := 0; written <= logs.Limit; written += len(line) {
		first.Write([]byte(line))
	}
	first.Close()

	if _, err := second.Write([]byte("after the rotation\n")); err != nil {
		t.Fatalf("writing after somebody else rotated: %v", err)
	}
	current, err := os.ReadFile(logs.Path(env))
	if err != nil {
		t.Fatalf("reading the current log: %v", err)
	}
	if !strings.Contains(string(current), "after the rotation") {
		t.Errorf("the second writer is still writing into the rotated file")
	}
}

func open(t *testing.T) (*logs.Log, pathx.Root) {
	t.Helper()
	env, err := pathx.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.Close() })
	log, err := logs.Open(env, nil)
	if err != nil {
		t.Fatalf("opening the log: %v", err)
	}
	t.Cleanup(func() { log.Close() })
	return log, env
}

func read(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}
