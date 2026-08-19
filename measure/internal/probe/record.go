package probe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Record is camp's own write-ahead record, read as data.
//
// Only the fields a driver asks about. The point of reading it here
// rather than through 'camp status' is that the assertions are about the
// record itself: that it survives until every place is clean, and that
// its phase says which boundary a killed run reached.
type Record struct {
	Hash     string   `json:"hash"`
	Env      string   `json:"env"`
	Live     string   `json:"live"`
	Staging  string   `json:"staging"`
	Phase    string   `json:"phase"`
	Stranded []string `json:"stranded,omitempty"`
	Detached []string `json:"detached,omitempty"`
	Mounts   []struct {
		Target  string `json:"target"`
		Staging string `json:"staging,omitempty"`
	} `json:"mounts"`

	// Path is where this was read from, so a message can name it.
	Path string `json:"-"`
}

// StateDir is where camp keeps its records, by the same rule camp uses.
func StateDir() string {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "camp")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "camp")
	}
	return filepath.Join(home, ".local", "state", "camp")
}

// RecordFor returns the record for one environment, and whether there is
// one.
//
// By environment and not by hash, because a driver that had to know the
// hash would have to ask camp for it -- and after a kill, asking camp is
// asking the thing under test. Every record in the directory is read and
// the one naming this environment is the answer; two would mean the
// machine has two compositions in one environment, which is a finding of
// its own and is reported as an error rather than resolved.
func RecordFor(environment string) (Record, bool, error) {
	entries, err := os.ReadDir(StateDir())
	if os.IsNotExist(err) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}

	var found []Record
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(StateDir(), entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return Record{}, false, err
		}
		var record Record
		if err := json.Unmarshal(data, &record); err != nil {
			return Record{}, false, fmt.Errorf("%s does not parse: %w", path, err)
		}
		if record.Env != environment {
			continue
		}
		record.Path = path
		found = append(found, record)
	}
	switch len(found) {
	case 0:
		return Record{}, false, nil
	case 1:
		return found[0], true, nil
	default:
		return Record{}, false, fmt.Errorf(
			"%s has %d records for %s, and one environment has one composition",
			StateDir(), len(found), environment)
	}
}

// Places is every path the record says a mount may be at: both ends of
// every planned mount, the two self-binds, and anything a previous run
// left stranded.
//
// The teardown's own order, and for the same reason: a mount is at one of
// its two places and the record cannot know which, so a driver checking
// only one of them would call a composition clean while it is standing
// where the record did not look.
func (r Record) Places() []string {
	var places []string
	for _, mount := range r.Mounts {
		places = append(places, mount.Target)
		if mount.Staging != "" {
			places = append(places, mount.Staging)
		}
	}
	places = append(places, r.Detached...)
	return append(places, r.Stranded...)
}
