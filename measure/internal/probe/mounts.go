// Package probe is what the two drivers share: the machine's mount table,
// the content of a tree, camp's own record, and the verdict they print.
//
// It depends on nothing outside the standard library, including camp
// itself. A driver that measured camp using camp's own parsing would
// agree with it by construction -- and one of the things being measured
// is whether camp's account of the machine is true.
package probe

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// SelfTable is the mount table of the calling process, which in these
// drivers is the machine's own.
const SelfTable = "/proc/self/mountinfo"

// Mount is one line of the table, reduced to what a driver compares.
type Mount struct {
	ID     int
	Parent int
	Point  string
	// Optional is the propagation field as the kernel wrote it: shared:N,
	// master:N, or empty for a mount that propagates nowhere.
	Optional string
	FSType   string
	Source   string
	// Line is the record verbatim, so a failure can quote what it saw
	// rather than a summary of it.
	Line string
}

// Table reads the mount table, keyed by mount id.
//
// The id is the key because it is the only thing about a mount that does
// not change: a mount moved to another path keeps it, which is exactly
// what the descriptor-safe teardown does on its way to unmounting
// something, and a driver keying by path would report that move as one
// mount gone and another arrived.
//
// The kernel's grammar, not Go's: fields are separated by one 0x20 and
// nothing else, the optional fields end at a literal "-", and pathnames
// carry \040 \011 \012 \134 for the four bytes the kernel escapes. A
// mount whose name contains a carriage return is a legal mount, and
// splitting on "whitespace" would shift every field after it.
func Table() (map[int]Mount, error) {
	data, err := os.ReadFile(SelfTable)
	if err != nil {
		return nil, err
	}
	table := map[int]Mount{}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, " ")
		if len(fields) < 10 {
			return nil, fmt.Errorf("a mount table record has %d fields: %s",
				len(fields), line)
		}
		separator := -1
		for index := 6; index < len(fields); index++ {
			if fields[index] == "-" {
				separator = index
				break
			}
		}
		if separator < 0 || separator+2 >= len(fields) {
			return nil, fmt.Errorf("a mount table record has no separator: %s", line)
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("a mount table record has no id: %s", line)
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("a mount table record has no parent: %s", line)
		}
		table[id] = Mount{
			ID:       id,
			Parent:   parent,
			Point:    unescape(fields[4]),
			Optional: strings.Join(fields[6:separator], " "),
			FSType:   fields[separator+1],
			Source:   unescape(fields[separator+2]),
			Line:     line,
		}
	}
	return table, nil
}

// unescape turns the four sequences the kernel writes back into bytes.
func unescape(field string) string {
	return strings.NewReplacer(
		`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(field)
}

// Gone returns the mounts that were in the first table and are not in the
// second, by id, deepest path first.
func Gone(before, after map[int]Mount) []Mount {
	var found []Mount
	for id, mount := range before {
		if _, still := after[id]; !still {
			found = append(found, mount)
		}
	}
	return deepestFirst(found)
}

// Arrived returns the mounts in the second table that were not in the
// first, deepest path first.
func Arrived(before, after map[int]Mount) []Mount {
	return Gone(after, before)
}

// Under returns every mount at or beneath a directory.
func Under(table map[int]Mount, prefix string) []Mount {
	var found []Mount
	for _, mount := range table {
		if mount.Point == prefix || strings.HasPrefix(mount.Point, prefix+"/") {
			found = append(found, mount)
		}
	}
	return deepestFirst(found)
}

// Points renders mounts as "<id> <path>", for a failure that has to name
// exactly what is standing.
func Points(mounts []Mount) string {
	var names []string
	for _, mount := range mounts {
		names = append(names, fmt.Sprintf("%d %s", mount.ID, mount.Point))
	}
	if len(names) == 0 {
		return "nothing"
	}
	return strings.Join(names, ", ")
}

func deepestFirst(mounts []Mount) []Mount {
	sort.Slice(mounts, func(i, j int) bool {
		left, right := strings.Count(mounts[i].Point, "/"), strings.Count(mounts[j].Point, "/")
		if left != right {
			return left > right
		}
		return mounts[i].Point < mounts[j].Point
	})
	return mounts
}
