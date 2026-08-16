package nsx

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// readSubordinate reads the range /etc/subuid or /etc/subgid gives an
// account: "<name>:<first>:<count>", one line per grant.
//
// Only the first grant is used. A second one would have to be spliced
// into the map by hand, and a partial map is worse than a refusal: it
// looks like it worked until a uid inside falls in the gap.
func readSubordinate(path, name string) (int, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) != 3 || fields[0] != name {
			continue
		}
		first, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		count, err := strconv.Atoi(fields[2])
		if err != nil || count <= 0 {
			continue
		}
		return first, count, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}
	return 0, 0, fmt.Errorf("no line for %q", name)
}
