package cli

// Recovery from the record, and from nothing else.
//
// Spec §12: down, status and explain read the recorded plan -- never a
// configuration that may have been edited, or deleted, while the
// composition was up. Measured before this existed, on a privileged
// composition that was up: with the configuration moved aside, 'camp
// status' and 'camp down' both answered "no .camp/config.yml here or in
// any parent directory", while the workspace was read-only for the whole
// machine and the record held the entire plan. There was no camp way
// back.
//
// So a composition can be named three ways that do not involve a
// configuration -- by its record, by its live path, or by standing in it
// -- and the fourth, a configuration, is one route to a record rather
// than the door everything else is behind.

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/pathx"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/report"
	"github.com/dlaszlo/camp/internal/state"
)

// recoveryFlags are the two ways to name a composition when there is no
// configuration to find it by.
func recoveryFlags(set *flag.FlagSet) (live, record *string) {
	live = set.String("live", "", "the composed tree's directory")
	record = set.String("record", "", "the composition's identifier, as 'camp list' prints it")
	return live, record
}

// selectRecord finds the composition a recovery command acts on.
//
// In order: an explicit record, an explicit live path, the record for a
// configuration named with -f, and otherwise the directory the caller is
// standing in. Not found is not an error -- "nothing is recorded here" is
// a true and useful answer, and status prints it.
func selectRecord(file, live, hash string) (state.Record, bool, error) {
	switch {
	case hash != "":
		record, found, err := state.Load(hash)
		if err != nil {
			return state.Record{}, false, wrap(err, ExitFailure, "")
		}
		if !found {
			return state.Record{}, false, failure(ExitNotFound, "",
				"there is no record %q. 'camp list' prints the ones there are.", hash)
		}
		return record, true, nil

	case live != "":
		real, err := pathx.Real(live)
		if err != nil {
			return state.Record{}, false, failure(ExitNotFound, "", "%v", err)
		}
		record, found, err := state.Load(plan.Hash(real))
		if err != nil {
			return state.Record{}, false, wrap(err, ExitFailure, "")
		}
		if !found {
			return state.Record{}, false, failure(ExitNotFound, "",
				"nothing is recorded for %s. 'camp list' prints the compositions "+
					"that are.", real)
		}
		return record, true, nil
	}

	// A configuration named with -f says which composition is meant, and
	// then it is the only one meant: falling back to whatever the current
	// directory belongs to would act on a composition nobody named.
	// Measured, because it happened -- a test that named a configuration in
	// a scratch directory reached the record of the real environment its
	// working directory sat in, and got as far as sudo.
	//
	// The file is read for its live path and for nothing else here, so one
	// that no longer parses still answers as long as the two fields naming
	// the tree survived: a mistyped section must never stand between
	// somebody and a teardown.
	if file != "" {
		cfg, err := config.Load(file)
		if err != nil && (cfg.Env == "" || cfg.Merged.Empty()) {
			return state.Record{}, false, failure(ExitUsage, "",
				"%s does not say which composition to act on, and it was named "+
					"with -f, so camp will not go looking for another one.", file)
		}
		real, err := pathx.Real(cfg.Live())
		if err != nil {
			return state.Record{}, false, nil
		}
		record, found, err := state.Load(plan.Hash(real))
		if err != nil {
			return state.Record{}, false, wrap(err, ExitFailure, "")
		}
		return record, found, nil
	}

	dir, err := os.Getwd()
	if err != nil {
		return state.Record{}, false, wrap(err, ExitFailure, "")
	}
	if resolved, err := pathx.Real(dir); err == nil {
		dir = resolved
	}

	matched := recordsHere(dir)
	switch len(matched) {
	case 0:
		return state.Record{}, false, nil
	case 1:
		return matched[0], true, nil
	}

	var names []string
	for _, record := range matched {
		names = append(names, fmt.Sprintf("  %s  %s (%s)",
			record.Hash, record.Live, record.Phase))
	}
	return state.Record{}, false, failure(ExitUsage, "",
		"%d compositions claim this directory, and camp will not choose "+
			"between them:\n%s\nName one with -record or -live.",
		len(matched), strings.Join(names, "\n"))
}

// noteUnreadableConfiguration says that the file is broken, before doing
// the work that does not need it.
//
// down tears down from the record. A configuration edited while the
// composition was up -- and the session: section adds a whole class of
// ways to get that wrong -- must not leave somebody behind mounts camp
// made and now refuses to remove. But saying nothing about a file that no
// longer parses would be its own trap, so it is reported here and then
// stepped over.
func noteUnreadableConfiguration(ctx *context, file string) {
	path := file
	if path == "" {
		start, err := os.Getwd()
		if err != nil {
			return
		}
		found, err := config.Find(start)
		if err != nil {
			return
		}
		path = found
	}

	var refused refusal.List
	if _, err := config.Load(path); err == nil || !errors.As(err, &refused) {
		return
	}
	fmt.Fprintf(ctx.err, "%s no longer reads as a configuration:\n\n%s\n"+
		"The teardown goes ahead anyway: it comes from this composition's "+
		"record, not from this file. What the file cannot say is what changed "+
		"while the composition was up, so the drift report is left out.\n\n",
		path, strings.TrimRight(report.Refusals(refused), "\n"))
}

// recordsHere returns the active compositions that claim a directory:
// those whose environment root or composed tree is this directory or
// holds it.
//
// A corrupt record claims nothing, because nothing in it can be read.
// 'camp list' is where those are reported, and status says how many it
// stepped over.
func recordsHere(dir string) []state.Record {
	var matched []state.Record
	for _, listing := range state.All() {
		if listing.Corrupt != nil || !listing.Record.Phase.Active() {
			continue
		}
		if under(dir, listing.Record.Env) || under(dir, listing.Record.Live) {
			matched = append(matched, listing.Record)
		}
	}
	return matched
}

func under(path, base string) bool {
	if base == "" {
		return false
	}
	return path == base || strings.HasPrefix(path, strings.TrimRight(base, "/")+"/")
}

// corruptRecords counts what recordsHere had to step over.
func corruptRecords() []state.Listing {
	var corrupt []state.Listing
	for _, listing := range state.All() {
		if listing.Corrupt != nil {
			corrupt = append(corrupt, listing)
		}
	}
	return corrupt
}

// standing reports whether any of a record's mounts is actually there.
//
// A record alone is not a composition: one left by a crash names mounts
// that may all be gone. What decides is the machine.
func standing(record state.Record, table []mountinfo.Entry) bool {
	for _, mount := range record.Mounts {
		if presence, _ := mount.Presence(table); presence != state.Gone {
			return true
		}
	}
	return false
}

// treeFromRecord describes the composition that is standing out of what
// was written down when it was built.
//
// Only the privileged mode leaves a record, so the description promises
// what that mode promises. The two blocks a plan renders -- the ownership
// note and the session -- are absent here on purpose: the first belongs
// to the namespace mode, and the privileged mode announces its session
// rather than applying it (§6).
func treeFromRecord(record state.Record) report.Tree {
	tree := report.Tree{
		Live:       record.Live,
		Upper:      record.Upper,
		Lower:      record.Workspace,
		Privileged: true,
	}
	for _, mount := range record.Mounts {
		if plan.Role(mount.Role) == plan.Artefact {
			tree.Generated = true
		}
		tree.Mounts = append(tree.Mounts, report.TreeMount{
			Path:   inTree(record.Live, mount.Target),
			Source: mount.Source,
			Role:   plan.Role(mount.Role),
			Kind:   plan.Kind(mount.Kind),
		})
	}
	return tree
}

// inTree renders a recorded target the way a description shows it:
// relative to the composed tree, or absolute when it is not in it.
func inTree(live, target string) string {
	if under(target, live) && target != live {
		return strings.TrimPrefix(target, strings.TrimRight(live, "/")+"/")
	}
	return target
}

// describeRecord prints what the machine says about a recorded
// composition: every recorded mount checked by path and by identity, and
// then the one line that says where this composition stands.
//
// Nothing here re-derives a plan from a configuration. What is on the
// machine was mounted from the record's plan, and that is what it is
// compared against.
func describeRecord(ctx *context, record state.Record, table []mountinfo.Entry) error {
	ctx.printf("record:  %s\n", state.Path(record.Hash))
	ctx.printf("live:    %s\n", record.Live)
	ctx.printf("phase:   %s, written %s\n", record.Phase, record.Age())

	counts := map[state.Presence]int{}
	ctx.printf("\n%d recorded mount(s), oldest first:\n", len(record.Mounts))
	for _, mount := range record.Mounts {
		presence, err := mount.Presence(table)
		counts[presence]++
		note := ""
		switch {
		case err != nil:
			note = fmt.Sprintf(" -- %v", err)
		case presence == state.Different:
			note = " -- not the object camp mounted"
		case presence == state.Unverified:
			note = " -- mounted, with no recorded identity to check it against"
		}
		ctx.printf("  %-10s %s%s\n", presence, mount.Target, note)
	}

	// The self-binds are listed after the plan and never verified by
	// identity: each one sits underneath the composition, so what a look at
	// its path finds is the tree standing on top of it.
	for _, path := range record.Detached {
		ctx.printf("  %-10s %s (bound onto itself so the move could not "+
			"propagate; whatever covers it answers for this path)\n",
			presenceOf(path, table), path)
	}

	ctx.printf("\n%s", verdict(record, counts))
	ctx.printf("%s", configDrift(record))

	if problem := disagreement(record, counts); problem != "" {
		ctx.printf("\n%s", problem)
		return failure(ExitPrecondition, "", "run 'camp down' to take it apart")
	}
	return nil
}

// disagreement names what the record and the machine say differently.
//
// This is the most useful thing status can say after a crash, because the
// phase says which boundary the run reached and the mounts say what it
// got done -- and the two together name the moment it stopped.
func disagreement(record state.Record, counts map[state.Presence]int) string {
	standing := counts[state.Same] + counts[state.Unverified] + counts[state.Different]
	switch {
	case counts[state.Different] > 0:
		return "camp will not take this apart on its own: some of what stands " +
			"at these paths is not what camp mounted.\n"
	case standing > 0 && record.Phase == state.Mounting:
		return "the record says 'mounting' and the composition is standing, so " +
			"the run stopped between the helper's work and the check that " +
			"follows it. Nothing is lost: the record names every mount that was " +
			"made.\n"
	case standing > 0 && record.Phase == state.Partial:
		return "the record says 'partial': a teardown or a rollback did not " +
			"finish, and what it could not remove is above.\n"
	case standing == 0 && record.Phase == state.Up:
		return "the record says 'up' and nothing of it is mounted: a teardown " +
			"took the composition apart and did not get as far as saying so, or " +
			"the machine was restarted, or something outside camp removed the " +
			"mounts. 'camp down' finishes the bookkeeping either way.\n"
	}
	return ""
}

func presenceOf(path string, table []mountinfo.Entry) state.Presence {
	if len(mountinfo.At(table, path)) == 0 {
		return state.Gone
	}
	return state.Unverified
}

func verdict(record state.Record, counts map[state.Presence]int) string {
	standing := counts[state.Same] + counts[state.Unverified] + counts[state.Different]
	switch {
	case standing == 0:
		return fmt.Sprintf("down: nothing the record names is mounted. The record is "+
			"still here, so nothing is lost -- 'camp down' finishes the teardown, "+
			"or 'camp forget %s' discards it.\n", record.Hash)
	case counts[state.Different] > 0:
		return fmt.Sprintf("partly up, and %d mount(s) are not what camp mounted. "+
			"camp will not remove those: whatever is standing there now belongs to "+
			"somebody else.\n", counts[state.Different])
	case standing == len(record.Mounts) && counts[state.Unverified] == 0:
		return "up: every recorded mount is present, and each is the object camp " +
			"mounted.\n"
	case standing == len(record.Mounts):
		// Said rather than glossed over. "Present" and "the object camp
		// mounted" are two different claims, and a record written before its
		// mounts were made can only support the first.
		return fmt.Sprintf("up: every recorded mount is present. %d of them "+
			"carry no recorded identity, so camp cannot say whether what stands "+
			"there is what it mounted: the record was written before those "+
			"mounts were made.\n", counts[state.Unverified])
	default:
		return fmt.Sprintf("partly up: %d of %d recorded mounts are present. "+
			"'camp down' removes what is left.\n", standing, len(record.Mounts))
	}
}

// configDrift reports whether the file the composition was built from has
// changed since. Separately, and never as part of the verdict: what is
// mounted is a fact about the machine, and what the file says today is a
// fact about the file.
func configDrift(record state.Record) string {
	if record.Config == "" || record.ConfigDigest == "" {
		return ""
	}
	data, err := os.ReadFile(record.Config)
	if err != nil {
		return fmt.Sprintf("\nthe configuration this was built from cannot be read "+
			"now (%v). That changes nothing about the teardown, which comes from "+
			"the record.\n", err)
	}
	if state.Digest(data) == record.ConfigDigest {
		return ""
	}
	return fmt.Sprintf("\n%s has changed since this composition was brought up. "+
		"What is mounted is what the record says, not what the file says now.\n",
		record.Config)
}
