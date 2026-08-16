// Package holders finds the processes that keep a composition from
// coming down.
//
// Read from /proc rather than asked of fuser(1). "fuser -m" on a bind
// mount reports every process using the underlying *device*, not the
// mount: asked which two processes were holding a bind of a directory on
// the root filesystem, it answered with a hundred and twenty. Walking
// /proc names the process, its command and the directory it is sitting
// in, and does not care what device anything is on.
//
// The usual holder is a shell -- or an editor, or any long-running tool
// -- whose working directory is inside the composed tree. It cannot be
// unmounted from under itself, which is worth reporting by name rather
// than as "device or resource busy".
package holders

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Holder is one process and what it is holding.
type Holder struct {
	PID     int
	Command string
	Kind    string // "cwd", "root", "exe" or "open file"
	Path    string
}

// Describe renders one holder for a person.
func (h Holder) Describe() string {
	return fmt.Sprintf("pid %d  %s  (%s: %s)", h.PID, h.Command, h.Kind, h.Path)
}

// Report is every holder found, and what could not be looked at.
type Report struct {
	Holders []Holder
	// Unreadable counts processes owned by another user. A non-root scan
	// cannot see their file descriptors, so a report saying "nothing is
	// holding it" is only as good as this number being zero.
	Unreadable int
}

// Any reports whether anything at all is holding the tree.
func (r Report) Any() bool { return len(r.Holders) > 0 }

// Caveat states what this report could not see, rather than implying it
// saw everything.
func (r Report) Caveat() string {
	if r.Unreadable == 0 {
		return ""
	}
	return fmt.Sprintf("%d process(es) belong to another user and could not be "+
		"inspected; run as root to see all of them", r.Unreadable)
}

// Find returns every process holding something at or below root.
func Find(root string) Report {
	report := Report{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return report
	}

	self := os.Getpid()
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		holder, denied := inspect(pid, root)
		if holder != nil {
			report.Holders = append(report.Holders, *holder)
		} else if denied {
			report.Unreadable++
		}
	}

	sort.Slice(report.Holders, func(i, j int) bool {
		return report.Holders[i].PID < report.Holders[j].PID
	})
	return report
}

// inspect looks at one process. It reports the first thing that is inside
// root, because listing every descriptor of the same process adds noise
// without adding an action.
func inspect(pid int, root string) (*Holder, bool) {
	denied := false

	for _, link := range []string{"cwd", "root", "exe"} {
		target, err := os.Readlink(fmt.Sprintf("/proc/%d/%s", pid, link))
		if err != nil {
			if os.IsPermission(err) {
				denied = true
			}
			continue
		}
		if link == "root" && target == "/" {
			continue
		}
		if under(target, root) {
			return &Holder{PID: pid, Command: command(pid), Kind: link, Path: target}, false
		}
	}

	descriptors, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return nil, denied || os.IsPermission(err)
	}
	for _, descriptor := range descriptors {
		target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, descriptor.Name()))
		if err != nil {
			continue
		}
		if under(target, root) {
			return &Holder{PID: pid, Command: command(pid), Kind: "open file", Path: target}, false
		}
	}
	return nil, denied
}

func command(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "?"
	}
	text := strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
	if text == "" {
		if comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
			text = strings.TrimSpace(string(comm))
		}
	}
	if len(text) > 80 {
		text = text[:80]
	}
	return text
}

func under(path, root string) bool {
	if path == root {
		return true
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
