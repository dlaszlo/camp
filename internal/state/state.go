// Package state records what is mounted, and reads what actually is.
//
// Two things are kept apart on purpose. The configuration says what you
// *want*; this package records what *happened*. "down" undoes what the
// record says was mounted, not what the current configuration would
// produce -- otherwise editing camp.yaml while a composition is up would
// leave mounts that nothing knows how to remove.
//
// The record is generated, machine-local and never edited by hand, so it
// lives under the user's state directory rather than beside the code.
package state

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Status is what a record says about a composition.
type Status string

const (
	Up   Status = "up"
	Down Status = "down"
)

// Record is one composition, as last acted on.
type Record struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Config     string   `json:"config"`
	Live       string   `json:"live"`
	Code       string   `json:"code"`
	Workspaces []string `json:"workspaces"`
	Private    string   `json:"private"`
	WorkDir    string   `json:"workdir"`
	Status     Status   `json:"status"`
	Mounts     []string `json:"mounts"`
	Created    []string `json:"created"`
	UpdatedAt  string   `json:"updated_at"`
	Version    string   `json:"tool_version"`
}

// Dir is where records live.
func Dir() string {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "camp")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "camp")
	}
	return filepath.Join(home, ".local", "state", "camp")
}

func path(id string) string { return filepath.Join(Dir(), id+".json") }

// Save writes the record, replacing any previous one.
//
// Written to a temporary file and renamed, so that an interrupted save
// cannot leave a record that parses cleanly and describes half a
// composition.
func (r Record) Save() error {
	r.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return fmt.Errorf("creating the state directory: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the record: %w", err)
	}

	target := path(r.ID)
	temporary := target + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing the record: %w", err)
	}
	if err := os.Rename(temporary, target); err != nil {
		return fmt.Errorf("replacing the record: %w", err)
	}
	return nil
}

// Forget removes the record and nothing else. Repositories, the
// configuration and every piece of content stay where they are.
func Forget(id string) error {
	err := os.Remove(path(id))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing the record: %w", err)
	}
	return nil
}

// Load reads one record, or reports that there is none.
func Load(id string) (Record, bool) {
	data, err := os.ReadFile(path(id))
	if err != nil {
		return Record{}, false
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, false
	}
	return record, true
}

// All returns every record, oldest identifier first.
func All() []Record {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		return nil
	}
	var records []Record
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if record, ok := Load(strings.TrimSuffix(entry.Name(), ".json")); ok {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records
}

// MountedTargets returns every mount point on this machine.
//
// Read from /proc/self/mountinfo rather than /proc/mounts: mountinfo
// lists each mount separately even when several share a source, which is
// exactly the case here, since a bind of a directory reports the whole
// device as its source.
func MountedTargets() map[string]bool {
	targets := map[string]bool{}
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return targets
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 4 {
			targets[unescape(fields[4])] = true
		}
	}
	return targets
}

// IsMounted reports whether one path is a mount point.
func IsMounted(target string) bool { return MountedTargets()[target] }

// unescape undoes mountinfo's octal escaping of space, tab, newline and
// backslash.
func unescape(field string) string {
	return strings.NewReplacer(
		`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`,
	).Replace(field)
}
