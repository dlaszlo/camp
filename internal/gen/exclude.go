// Package gen produces the two artefacts that need git knowledge, and
// keeps that knowledge out of camp's core.
//
// The core needs git in exactly two places: the exclude payload, and the
// list of what a repository contributes at an islands mount. Both are
// produced by one generation step, with a shipped default. A composition
// that is not git-based simply does not list one, and then it has no
// exclude at all -- which plan says plainly rather than leaving the
// defence out quietly.
//
// Everything here runs in the prepare phase: before anything is mounted,
// always as the invoking user. What it produces is then validated as
// hostile data before a single mount is made from it.
package gen

import (
	"fmt"
	"os"
	"strings"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/enc"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
)

// MarkerPrefix opens the block camp generates. The rest of the line names
// the composition and says not to edit it, because the file it appears in
// looks exactly like the repository's own.
const MarkerPrefix = "# camp:generated "

// Marker renders the whole first line of the generated block.
func Marker(hash string) string {
	return MarkerPrefix + hash + " -- do not edit; regenerated at every up"
}

// ExcludeLines derives the patterns, from the raw lower root listing and
// the mount targets.
//
//	exclude = (workspace root entries) - (allow_overlap) + (every mount target)
//
// **Coarse, one line per root name, and every line anchored with a
// leading slash.** Three things make that the right shape rather than the
// lazy one.
//
// A file born in the workspace mid-session -- ordinary editing, arriving
// instantly through the binds, which are live views rather than snapshots
// -- is covered by its directory's line automatically. A file-level
// enumeration would not have a line for it, and 'git add .' could stage
// it in exactly the window camp cannot re-check.
//
// It is lossless because of the zero-overlap invariant: no name exists on
// both sides, so a root line can never hide code-side content -- and the
// gate re-verifies that invariant at every up, so the exclude never runs
// on a tree its shape is not valid for.
//
// The leading slash is load-bearing, not cosmetic. The gate compares root
// entries only, so a workspace root name and a same-named directory deep
// in the code repository never meet in that comparison. Measured: an
// unanchored line 'scripts' hides new files under the code repository's
// real frontend/scripts and deploy/mcp-server/scripts, and no gate ever
// fires. The anchor is the only guard for that class.
func ExcludeLines(cfg config.Config, built plan.Plan) ([]string, refusal.List) {
	var refused refusal.List
	seen := map[string]bool{}
	var lines []string

	add := func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		lines = append(lines, path)
	}

	for _, entry := range built.LowerRoot {
		if cfg.AllowsOverlap(entry.Name) {
			continue
		}
		if strings.ContainsAny(entry.Name, "\n\r") {
			refused.Add("exclude-name-newline",
				"the workspace root entry %q contains a line break and cannot be "+
					"written as a gitignore pattern at all. The attempt would "+
					"silently ignore that name and hide two unrelated ones instead.",
				entry.Name)
			continue
		}
		add(entry.Name)
	}

	for _, mount := range built.Mounts {
		if !mount.InLive || mount.Rel.Empty() {
			continue
		}
		components := mount.Rel.Components()
		if len(components) == 1 && cfg.AllowsOverlap(components[0]) {
			continue
		}
		add(mount.Rel.String())
	}

	enc.SortNames(lines)
	patterns := make([]string, 0, len(lines))
	for _, line := range lines {
		patterns = append(patterns, "/"+escapePattern(line))
	}
	return patterns, refused
}

// escapePattern protects the characters gitignore reads as syntax.
//
// The set is measured: a backslash, brackets, star, question mark, and a
// trailing space. A '#' or '!' needs nothing once the line starts with a
// slash, which every line here does.
func escapePattern(path string) string {
	var out strings.Builder
	for index := 0; index < len(path); index++ {
		switch character := path[index]; character {
		case '\\', '[', ']', '*', '?':
			out.WriteByte('\\')
			out.WriteByte(character)
		default:
			out.WriteByte(character)
		}
	}
	rendered := out.String()
	if strings.HasSuffix(rendered, " ") {
		rendered = rendered[:len(rendered)-1] + `\ `
	}
	return rendered
}

// ExcludePayload assembles the whole file that will be mounted.
//
// The repository's own exclude bytes come first, unchanged and complete,
// and then camp's block. If the repository's file is nonempty and does
// not end in a newline, exactly one is inserted -- direct concatenation
// would fuse the marker into the last pattern and both would stop
// meaning what they say. An empty file contributes nothing.
//
// Verification later compares the mounted file against this whole
// payload, byte for byte. A marker-prefix match alone would accept a
// payload whose repository half had been dropped.
func ExcludePayload(existing []byte, hash string, patterns []string) []byte {
	var out strings.Builder
	if len(existing) > 0 {
		out.Write(existing)
		if existing[len(existing)-1] != '\n' {
			out.WriteByte('\n')
		}
	}
	out.WriteString(Marker(hash))
	out.WriteByte('\n')
	for _, pattern := range patterns {
		out.WriteString(pattern)
		out.WriteByte('\n')
	}
	return []byte(out.String())
}

// ReadExisting reads the repository's own exclude.
//
// A missing file, or a missing .git/info, is refused with the two
// commands that repair it. camp will not create them: that would be
// writing into a repository, and the first invariant does not bend for
// convenience.
func ReadExisting(path string) ([]byte, refusal.List) {
	var refused refusal.List
	data, err := os.ReadFile(path)
	if err == nil {
		return data, refused
	}
	if !os.IsNotExist(err) {
		refused.Add("exclude-unreadable", "%s could not be read: %v.", path, err)
		return nil, refused
	}

	directory := strings.TrimSuffix(path, "/exclude")
	refused.Add("exclude-missing",
		"%s does not exist.\n"+
			"A repository initialised from an empty template has neither the file "+
			"nor the directory above it. camp generates the exclude and mounts it "+
			"over the composed tree's copy, so the copy has to be there to mount "+
			"over -- and camp will not create it, because it never writes into a "+
			"repository. Run these two commands:\n"+
			"  mkdir -p %s\n  touch %s", path, directory, path)
	return nil, refused
}

// DescribeExclude renders the payload for a plan, so that what will be
// mounted can be read before it is.
func DescribeExclude(patterns []string) string {
	if len(patterns) == 0 {
		return "  (no patterns)\n"
	}
	var b strings.Builder
	for _, pattern := range patterns {
		fmt.Fprintf(&b, "  %s\n", pattern)
	}
	return b.String()
}
