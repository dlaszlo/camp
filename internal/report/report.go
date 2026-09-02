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
	"time"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/mountx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/preflight"
	"github.com/dlaszlo/camp/internal/refusal"
)

// Refusals renders every refusal, one paragraph each, numbered when there
// is more than one so that a reader can talk about "the second one".
//
// One paragraph per problem, not per subject: a check that fired for nine
// mounts is one refusal here, naming the nine. Nine copies of the same
// three paragraphs would bury the only thing in them that differs.
func Refusals(list refusal.List) string {
	list = list.Merge()
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
// happens, with the reason it exists -- and how the session it starts
// ends, with the grace the caller's session package holds.
func Plan(p plan.Plan, grace time.Duration) string {
	var b strings.Builder

	fmt.Fprintf(&b, "composition %s\n", p.Hash)
	fmt.Fprintf(&b, "  configuration: %s\n", p.Config.Source)
	fmt.Fprintf(&b, "  environment:   %s\n", p.Config.Env)
	fmt.Fprintf(&b, "  composed tree: %s\n", p.Live)
	fmt.Fprintf(&b, "  work (disposable):  %s\n", p.Work)
	fmt.Fprintf(&b, "  storage (kept):     %s\n\n", p.Storage)

	b.WriteString(prepareCommands(p.Config))

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
	b.WriteString("how the session ends:\n\n  " + wrap(Ending(grace), "  ") + "\n\n")

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

// prepareCommands renders the `prepare:` list, and says plainly that
// planning did not run it.
//
// It is here because it is the one part of a real run that changes
// something before any mount: these are the environment's own programs,
// and a plan that listed the mounts and said nothing about them would
// describe a different run from the one that follows. Argv vectors are
// printed quoted, one element at a time, because a vector joined by
// spaces cannot be told apart from a vector whose elements contain them.
func prepareCommands(cfg config.Config) string {
	if len(cfg.Prepare) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("prepare, before anything is composed:\n\n")
	for _, command := range cfg.Prepare {
		fmt.Fprintf(&b, "%2d. %q", command.Index+1, command.Command)
		if command.Timeout > 0 {
			fmt.Fprintf(&b, "   timeout %s", command.Timeout)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n    These are the environment's own programs. camp runs them as you,\n" +
		"    after the locks are taken and before this plan is derived, and the\n" +
		"    first one that does not succeed refuses the composition. They can\n" +
		"    write wherever you can, a repository included.\n" +
		"    'camp plan' has not run them.\n\n")
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
//
// The composed tree is made with the kernel's mount API rather than with
// mount(2), because that is the only way to hand it the layers as
// descriptors instead of as names it resolves itself. It is shown as it
// happens: a filesystem context, one call per operand, and a mount that
// exists before it is anywhere.
func Syscalls(p plan.Plan) string {
	var b strings.Builder
	for _, mount := range p.Mounts {
		switch mount.Kind {
		case plan.Overlay:
			// From the description the mount is performed from, so what this
			// prints is what the kernel will be given rather than a fourth
			// account of it. 'camp plan' is read to decide whether to run the
			// thing it describes, so a sequence that has drifted from the one
			// camp performs is worse than no sequence at all.
			described := mountx.DescribeOverlay(mount)
			fmt.Fprintf(&b, "  fsopen(%q, FSOPEN_CLOEXEC)\n", described.FSType)
			for _, step := range described.Steps {
				if step.Flag() {
					fmt.Fprintf(&b, "  fsconfig(fs, FSCONFIG_SET_FLAG, %q, NULL, 0)\n", step.Key)
					continue
				}
				fmt.Fprintf(&b, "  fsconfig(fs, FSCONFIG_SET_FD, %q, NULL, fd of %s)\n",
					step.Key, step.Path)
			}
			b.WriteString("  fsconfig(fs, FSCONFIG_CMD_CREATE, NULL, NULL, 0)\n")
			b.WriteString("  fsmount(fs, FSMOUNT_CLOEXEC, 0)\n")
			fmt.Fprintf(&b, "  move_mount(mount, \"\", fd of %s, \"\", MOVE_MOUNT_F_EMPTY_PATH|MOVE_MOUNT_T_EMPTY_PATH)\n",
				mount.Target)
		default:
			// By name, inside the session's own namespace, where nothing but
			// this process can have mounted anything between the check and
			// the mount.
			fmt.Fprintf(&b, "  mount(%q, %q, \"\", MS_BIND, \"\")\n",
				mount.Source, mount.Target)
		}
		if mount.Kind == plan.BindRO {
			fmt.Fprintf(&b, "  mount(\"\", %q, \"\", MS_REMOUNT|MS_BIND|MS_RDONLY|<locked flags>, \"\")\n",
				mount.Target)
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
		// A line's own indent is kept, and every line it folds onto keeps
		// it too. That is the same reason the line breaks are kept: an
		// indented line is a command somebody is meant to copy, and one
		// flattened into the paragraph around it is one nobody can.
		own := paragraph[:len(paragraph)-len(strings.TrimLeft(paragraph, " \t"))]
		var lines []string
		current := ""
		for _, word := range strings.Fields(paragraph) {
			switch {
			case current == "":
				current = own + word
			case len(current)+1+len(word) > width:
				lines = append(lines, current)
				current = own + word
			default:
				current += " " + word
			}
		}
		out = append(out, append(lines, current)...)
	}
	joined := strings.Join(out, "\n"+prefix)
	// A folded blank line would otherwise carry the prefix as trailing
	// whitespace.
	return strings.Join(trimRightEach(strings.Split(joined, "\n")), "\n")
}

func trimRightEach(lines []string) []string {
	for index, line := range lines {
		lines[index] = strings.TrimRight(line, " \t")
	}
	return lines
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

# Where the composed tree appears, inside env:. camp creates it if it is
# not there -- git cannot record an empty directory, so a fresh clone of
# an environment never brings it -- and it has to be empty: an overlay
# laid over content hides that content for the whole session.
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

# The environment's own programs, run before anything is composed and
# able to refuse the composition. camp runs them as you, after it holds
# the locks and before it derives the plan, and it takes nothing from
# them but whether they succeeded. Leave the key out if you have none.
#
# prepare:
#   - command: [bin/check-the-checkouts]
#     timeout: 120

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
# start -- the processes inside -- and nothing about the tree, which the
# keys above describe.
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
