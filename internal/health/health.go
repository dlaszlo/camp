// Package health is what doctor looks at beyond the machine's own
// capabilities: the state this environment has accumulated.
//
// Everything here is read-only and everything here is a report. Nothing
// in this package refuses anything -- doctor's job is to say what is
// there, including the things that are perfectly fine to leave alone.
package health

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dlaszlo/camp/internal/compose"
	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/gitwire"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/mountx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/reports"
)

// Note is one observation, with what to do about it when there is
// anything to do.
type Note struct {
	Subject string
	Detail  string
	Action  string
}

// Look gathers everything doctor reports about an environment.
func Look(cfg config.Config, built plan.Plan, table []mountinfo.Entry) []Note {
	var notes []Note
	notes = append(notes, locale())
	notes = append(notes, filesystems(cfg, built, table)...)
	notes = append(notes, orphans(cfg)...)
	notes = append(notes, leftoverWork(cfg)...)
	notes = append(notes, worktrees(cfg)...)
	notes = append(notes, sessionReports(cfg)...)
	return notes
}

// locale reports the environment camp's own command output runs in.
//
// Command output is translated on this machine, and camp decides nothing
// by reading a message -- every parsed command runs under LC_ALL=C and
// every question about state is asked of /proc. This note exists so that
// the habit is visible rather than assumed.
func locale() Note {
	current := os.Getenv("LC_ALL")
	if current == "" {
		current = os.Getenv("LANG")
	}
	if current == "" {
		current = "not set"
	}
	return Note{
		Subject: "locale",
		Detail: fmt.Sprintf("%s; camp runs every command it parses under "+
			"LC_ALL=C and asks /proc rather than reading messages", current),
	}
}

// filesystems reports what the environment sits on, and which flags a
// read-only remount will have to replicate.
//
// Reported, never refused. A nosuid filesystem is supported -- the flags
// are replicated -- and a noexec one is worth knowing about because
// scripts in the composed tree will not run, which is information and not
// a reason to stop.
func filesystems(cfg config.Config, built plan.Plan, table []mountinfo.Entry) []Note {
	var notes []Note
	seen := map[string]bool{}

	for _, path := range []string{cfg.Env, cfg.UpperPath(), cfg.LowerPath(), built.Live} {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		entry, found := mountinfo.Containing(table, path)
		if !found {
			continue
		}
		flags := mountx.DescribeFlags(mountx.LockedFlags(entry))
		note := Note{
			Subject: "filesystem: " + path,
			Detail:  fmt.Sprintf("%s on %s, locked flags %s", entry.FSType, entry.Point, flags),
		}
		if entry.Has("noexec") {
			note.Action = "this filesystem is mounted noexec, so nothing in the " +
				"composed tree can be executed from it. camp replicates the flag " +
				"because a read-only remount inside a namespace has to; it does " +
				"not stop the composition."
		}
		notes = append(notes, note)
	}
	return notes
}

// orphans lists storage directories whose composition no longer exists.
//
// Renaming the composed tree's directory changes the hash the storage is
// named from, which orphans the old one: nothing is lost, and nothing
// points at it either. camp never removes storage -- it holds unfinished
// worktrees -- so this is a list for a person to act on.
func orphans(cfg config.Config) []Note {
	var notes []Note
	root := filepath.Join(cfg.CampDir(), "storage")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		directory := filepath.Join(root, entry.Name())
		live, configPath, err := compose.ReadMarker(directory)
		if err != nil {
			notes = append(notes, Note{
				Subject: "storage: " + directory,
				Detail:  fmt.Sprintf("no readable %s: %v", compose.MarkerName, err),
				Action: "camp cannot say which composition this belonged to, so " +
					"it leaves it alone. Look at what is inside before removing it.",
			})
			continue
		}
		if _, err := os.Stat(live); err == nil {
			continue
		}
		notes = append(notes, Note{
			Subject: "storage: " + directory,
			Detail: fmt.Sprintf("its composed tree %s no longer exists (from %s)",
				live, configPath),
			Action: "Orphaned -- most likely the composed tree was renamed, which " +
				"changes the name this storage is derived from. Nothing was lost. " +
				"camp will not remove it, because storage holds worktrees and " +
				"machine-local files; move what you want out of it and delete the " +
				"rest yourself.",
		})
	}
	return notes
}

// leftoverWork explains the mode-000 directory a finished session leaves
// behind.
//
// OverlayFS creates a directory of its own inside the workdir it is
// given, and makes it mode 000 so that nothing wanders in. The namespace
// mode has no teardown step that could remove it -- the kernel discards
// the mounts when the session ends, but the directory is on the real
// filesystem and outlives them, and while the overlay is still mounted it
// is in use and cannot be removed anyway. So the next 'camp run' sweeps
// it.
//
// Somebody looking at their own filesystem meanwhile finds a directory
// they cannot open, owned by them, inside a tool's scratch space. That is
// alarming for exactly as long as nobody explains it.
func leftoverWork(cfg config.Config) []Note {
	root := filepath.Join(cfg.CampDir(), "work")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var notes []Note
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		leftover := filepath.Join(root, entry.Name(), "work", "work")
		info, err := os.Lstat(leftover)
		if err != nil || !info.IsDir() {
			continue
		}
		live, _, markerErr := compose.ReadMarker(filepath.Join(root, entry.Name()))
		belongs := "a composition camp could not identify"
		if markerErr == nil {
			belongs = live
		}
		notes = append(notes, Note{
			Subject: "work directory: " + leftover,
			Detail: fmt.Sprintf("mode %v, left by the kernel after a session on %s",
				info.Mode().Perm(), belongs),
			Action: "This is OverlayFS's own directory inside the workdir camp " +
				"gave it, and the mode is the kernel's doing, not camp's -- it is " +
				"made unreadable so that nothing wanders into it. It is empty of " +
				"anything you own. The next 'camp run' in this environment removes " +
				"it; camp does not remove it sooner because while the composition " +
				"is up the directory is in use, and a namespace session has no " +
				"teardown step of its own.",
		})
	}
	return notes
}

// worktrees lists registrations git will prune while nobody is looking.
func worktrees(cfg config.Config) []Note {
	code, isGit := gitwire.Open(cfg.UpperPath())
	if !isGit {
		return nil
	}
	registered, err := code.Worktrees()
	if err != nil {
		return nil
	}

	var notes []Note
	for _, worktree := range registered {
		if !worktree.Prunable {
			continue
		}
		notes = append(notes, Note{
			Subject: "worktree: " + worktree.Path,
			Detail:  "git considers this registration prunable: " + worktree.Reason,
			Action: fmt.Sprintf("If the checkout is still there, repair the "+
				"registration so it stops depending on a composition:\n"+
				"  git -C %s worktree repair <the checkout's real path>\n"+
				"Left alone, git removes the registration at the next auto-gc "+
				"after gc.worktreePruneExpire (three months by default). "+
				"Committed work survives on its branch; uncommitted work becomes "+
				"plain files nothing knows about.", cfg.UpperPath()),
		})
	}
	return notes
}

// sessionReports lists what namespace sessions have left behind.
func sessionReports(cfg config.Config) []Note {
	var notes []Note
	unseen := reports.Unseen(cfg.CampDir())
	seen := reports.Seen(cfg.CampDir())

	if len(unseen) > 0 {
		notes = append(notes, Note{
			Subject: "session reports",
			Detail:  fmt.Sprintf("%d not yet read: %s", len(unseen), strings.Join(unseen, ", ")),
			Action:  "The next camp command in this environment prints them once.",
		})
	}
	if len(seen) > 0 {
		notes = append(notes, Note{
			Subject: "session reports",
			Detail:  fmt.Sprintf("%d already read, kept at %s", len(seen), reports.Dir(cfg.CampDir())),
		})
	}
	return notes
}

// Render turns the notes into text.
func Render(notes []Note) string {
	if len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	for _, note := range notes {
		fmt.Fprintf(&b, "  %s\n      %s\n", note.Subject, note.Detail)
		if note.Action != "" {
			for _, line := range strings.Split(note.Action, "\n") {
				fmt.Fprintf(&b, "      %s\n", line)
			}
		}
	}
	return b.String()
}
