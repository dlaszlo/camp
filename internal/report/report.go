// Package report renders what camp found, for a person.
//
// The standard every message here is held to: name the path, say what is
// true on each side, say which side matters, give the exact command that
// repairs it, and say whose move it is. The reader has not read the
// specification and should not have to. "Overlap detected" is not an
// acceptable message; the refusal that names both files and both ways out
// is the product.
//
// Rendering lives apart from deciding on purpose. Every check returns
// data, this package turns it into text, and nothing that decides
// anything reaches for a terminal.
package report

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/mountx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/preflight"
	"github.com/dlaszlo/camp/internal/refusal"
)

// Refusals renders every refusal, one paragraph each, numbered when there
// is more than one so that a reader can talk about "the second one".
func Refusals(list refusal.List) string {
	if list.Empty() {
		return ""
	}
	var b strings.Builder
	for index, item := range list {
		if len(list) > 1 {
			fmt.Fprintf(&b, "%d. ", index+1)
		}
		b.WriteString(indent(item.Message, len(list) > 1))
		b.WriteString("\n\n")
	}
	return b.String()
}

// indent lines up the continuation lines of a numbered paragraph.
func indent(text string, numbered bool) string {
	if !numbered {
		return text
	}
	lines := strings.Split(text, "\n")
	for i := 1; i < len(lines); i++ {
		lines[i] = "   " + lines[i]
	}
	return strings.Join(lines, "\n")
}

// Plan renders the whole derived sequence: every mount, in the order it
// happens, with the reason it exists.
func Plan(p plan.Plan) string {
	var b strings.Builder

	fmt.Fprintf(&b, "composition %s\n", p.Hash)
	fmt.Fprintf(&b, "  configuration: %s\n", p.Config.Source)
	fmt.Fprintf(&b, "  environment:   %s\n", p.Config.Env)
	fmt.Fprintf(&b, "  composed tree: %s\n", p.Live)
	fmt.Fprintf(&b, "  mode:          %s\n", p.Mode)
	fmt.Fprintf(&b, "  work (disposable):  %s\n", p.Work)
	fmt.Fprintf(&b, "  storage (kept):     %s\n\n", p.Storage)

	b.WriteString("mount sequence, in order:\n\n")
	for index, mount := range p.Mounts {
		fmt.Fprintf(&b, "%2d. %s\n", index+1, mount.Describe())
		fmt.Fprintf(&b, "    why: %s\n", wrap(mount.Why, "         "))
		fmt.Fprintf(&b, "    from: %s\n\n", origin(mount))
	}

	if len(p.Warnings) > 0 {
		b.WriteString("worth knowing (none of these stop a composition):\n")
		for _, warning := range p.Warnings {
			fmt.Fprintf(&b, "  %s\n", warning)
		}
		b.WriteString("\n")
	}

	if session := Session(p, "the session's environment:"); session != "" {
		b.WriteString(session)
		b.WriteString("\n")
	}

	if _, has := p.Config.GenerationStep(); !has {
		b.WriteString("no generation step: this composition has no exclude at all.\n" +
			"  git run inside the composed tree will list every workspace name as\n" +
			"  untracked, and 'git add .' will stage their content into the code\n" +
			"  repository. The read-only mounts still stop writes; nothing stops\n" +
			"  reads. Add '- git_exclude' to steps: if this composition is\n" +
			"  git-based.\n\n")
	}

	return b.String()
}

// Expansion renders what the generation step produces: the islands each
// islands mount will really carry, and the exclude patterns.
//
// Printed rather than summarised, because the whole point of the coarse
// exclude is that a person can read it and see what it covers.
func Expansion(p plan.Plan, out gen.Output) string {
	var b strings.Builder

	if len(p.IslandsMounts) > 0 {
		b.WriteString("islands, derived:\n")
		b.WriteString(gen.Describe(p, out))
		b.WriteString("\n")
	}
	if len(out.Patterns) > 0 {
		fmt.Fprintf(&b, "the generated exclude, mounted over %s:\n",
			filepath.Join(p.Live, ".git", "info", "exclude"))
		fmt.Fprintf(&b, "  %s\n", gen.Marker(p.Hash))
		b.WriteString(gen.DescribeExclude(out.Patterns))
		b.WriteString("  Every line is anchored with a leading slash. The gate " +
			"compares root\n  entries only, so an unanchored name would also hide " +
			"a same-named\n  directory deep in the code repository, and no gate " +
			"would ever fire.\n\n")
	}
	for _, note := range out.Notes {
		fmt.Fprintf(&b, "note: %s\n", wrap(note, "      "))
	}
	if len(out.Notes) > 0 {
		b.WriteString("\n")
	}
	return b.String()
}

// Syscalls renders the mount calls a run would make, in order, for
// somebody who wants to see the plan in the kernel's own terms.
func Syscalls(p plan.Plan) string {
	var b strings.Builder
	for _, mount := range p.Mounts {
		switch mount.Kind {
		case plan.Overlay:
			fmt.Fprintf(&b, "  mount(\"overlay\", %q, \"overlay\", 0, %q)\n",
				mount.Target, mountx.Options(mount))
		default:
			fmt.Fprintf(&b, "  mount(%q, %q, \"\", MS_BIND, \"\")\n",
				mount.Source, mount.Target)
			if mount.Kind == plan.BindRO {
				fmt.Fprintf(&b, "  mount(\"\", %q, \"\", MS_REMOUNT|MS_BIND|MS_RDONLY|<locked flags>, \"\")\n",
					mount.Target)
			}
		}
		fmt.Fprintf(&b, "  mount(\"\", %q, \"\", MS_PRIVATE, \"\")\n", mount.Target)
	}
	return b.String()
}

func origin(mount plan.Mount) string {
	if mount.Step < 0 {
		return fmt.Sprintf("the frame (%s) -- no configuration can move, weaken "+
			"or leave this out", mount.Role)
	}
	return fmt.Sprintf("steps: item %d (%s)", mount.Step+1, mount.Role)
}

// wrap folds a sentence at a sensible width with a continuation indent,
// so a long reason stays readable in a terminal.
//
// Line breaks that are already in the text are kept. They are there
// because somebody put a command on a line of its own, and a command
// folded into a paragraph is a command nobody can copy.
func wrap(text, prefix string) string {
	const width = 68
	var out []string
	for _, paragraph := range strings.Split(text, "\n") {
		var lines []string
		current := ""
		for _, word := range strings.Fields(paragraph) {
			switch {
			case current == "":
				current = word
			case len(current)+1+len(word) > width:
				lines = append(lines, current)
				current = word
			default:
				current += " " + word
			}
		}
		lines = append(lines, current)
		out = append(out, lines...)
	}
	return strings.Join(out, "\n"+prefix)
}

// Checks renders a preflight report.
func Checks(checks []preflight.Check) string {
	var b strings.Builder
	width := 0
	for _, check := range checks {
		if len(check.Name) > width {
			width = len(check.Name)
		}
	}
	for _, check := range checks {
		fmt.Fprintf(&b, "  %-4s %-*s  %s\n", check.Symbol(), width, check.Name, check.Detail)
		if check.Hint != "" && !check.OK {
			fmt.Fprintf(&b, "       %s\n", wrap(check.Hint, "       "))
		}
	}
	return b.String()
}

// ConfigTemplate renders the skeleton 'camp init' writes: the shape of
// the configuration language, commented, with every path left to be
// filled in.
func ConfigTemplate(env string) string {
	return fmt.Sprintf(`# How this working directory is composed.
#
# camp puts several git repositories into one tree without any of them
# learning about the others. Writes land in the code repository or in
# machine-local storage -- never in the workspace.

# The one absolute path in the file. Everything else is relative to it.
env: %s

# Where the composed tree appears, inside env:. It has to exist already,
# and it has to be empty.
merged: CHANGE-ME-live

# The participants. A source addresses one of these by name.
repositories:
  - { name: workspace, path: CHANGE-ME-workspace }
  - { name: code,      path: CHANGE-ME }

overlayfs:
  # The workspace: read-only underneath. A list, because several lowers
  # are a later iteration; exactly one entry is accepted today.
  lower: [workspace]
  # The code repository: on top, and the only place ordinary writes land.
  upper: code

# Names allowed to exist in both roots. Everything else that appears on
# both sides stops the composition before anything is mounted, and the
# refusal says which side would win. Each repository needs its own
# .gitignore, so that one is expected.
allow_overlap: [.gitignore]

# The mount sequence. This is an ordered list and its order is the mount
# order: an earlier mount's target may not sit inside a later one's, or
# the later would silently cover the earlier.
steps:
  # Both repositories have a .git, and directories merge -- without this
  # bind the two histories would union and refs/heads/main could resolve
  # to the wrong one. camp never adds it for you: leaving it out is
  # caught by the overlap gate, by a rule that knows nothing about git.
  - mount_rw:
      - { source: "code/.git", target: ".git" }

  # The shipped generation step: reads git, produces the exclude and the
  # islands expansions, and binds the exclude over the composed tree's
  # copy. Without it this composition has no exclude at all.
  - git_exclude

# Everything below configures the session 'camp run' and 'camp shell'
# start, and nothing about the tree, which is the same in both modes.
# 'camp up' starts no session: it applies none of this and prints one line
# saying so, rather than leaving you to wonder whether it took effect.
#
# session.environment is not env: at the top of this file. env: names the
# environment *root directory*; this declares the *process environment*
# the session's workload receives, and through ordinary inheritance
# everything descended from it.
#
# In a value, $NAME and ${NAME} insert what a name held in the environment
# you started camp in, $$ is one literal dollar, and $CAMP_LIVE is the
# composed tree. There is no shell in any of it: nothing else is expanded,
# and a name that is not set refuses rather than quietly becoming empty
# text. Names beginning CAMP_ are camp's own, and so is PWD.
#
# Why you might want this: a session maps only your own uid, so files
# owned by anyone else appear as 'nobody', and a program that validates
# the owner of a configuration file refuses it. ssh is the one most people
# meet. docs/install.md, under "ssh inside a session", has the complete
# worked arrangement for OpenSSH -- including the launcher directory that
# a prepended PATH reaches.
#
# Uncomment from here down and edit:
#
# session:
#   # How you are mapped inside. The default -- your own uid mapped to
#   # itself, with the mount capability dropped before anything you asked
#   # for runs -- is what you get by leaving this out, and it is what you
#   # want unless you know otherwise. The other route needs the uidmap
#   # package installed. It stays commented when you uncomment the rest.
#   # identity: uidmap
#
#   environment:
#     SOME_TOOL_OPTIONS: "--config ${HOME}/.config/some-tool"
#     PATH: "$CAMP_LIVE/.workspace/bin:$PATH"
`, env)
}

// ConfigSummary is the one-line description of what a configuration
// composes, for the top of a report.
func ConfigSummary(cfg config.Config) string {
	return fmt.Sprintf("%s (workspace) + %s (code) -> %s",
		cfg.LowerPath(), cfg.UpperPath(), cfg.Live())
}
