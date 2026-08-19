// Command killmatrix interrupts the privileged half of 'camp up' at each
// boundary and requires camp to recover from its record alone.
//
// What it answers, and why it cannot be answered by reading the code:
// camp's write-ahead record claims to be sufficient on its own. A run
// killed anywhere between the first mount and the front end's save leaves
// the machine in one of eight states, and in every one of them the record
// -- with the configuration deleted -- has to be enough to take the whole
// composition apart, or to say exactly what it could not remove. Whether
// that is true is a property of the machine after a kill, and the only way
// to know it is to kill it.
//
// For each boundary it: arms the barrier to kill the helper where it
// stands, runs 'camp up', renames the configuration away, and runs
// 'camp down'. Then it requires, from the review's own list:
//
//   - every mount id camp created has disappeared;
//   - no unrelated mount id has disappeared;
//   - the repositories and the storage hash identically before and after;
//   - the record survives until every place is clean;
//   - anything that could not be removed is named exactly.
//
// It also requires that 'camp status', with no configuration on disk,
// still describes the composition from the record.
//
// # Running it
//
// It needs a real terminal, because sudo asks for a password on one and
// cannot ask anywhere else, and it needs a camp built with the barriers:
//
//	go build -tags camptest -o ~/campcheck/camp-barriers ./cmd/camp
//	go run ./killmatrix -env ~/campcheck -camp ~/campcheck/camp-barriers
//
// Run it as yourself and not with sudo. camp's front end refuses to run
// as root, on purpose, and this drives the front end; the privileged
// steps are the ones camp elevates for itself.
//
// It mounts and unmounts a real composition many times. Point it at a
// scratch environment, never at one you are working in.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"camp/measure/internal/probe"
)

func main() { os.Exit(run()) }

// boundary is one place the helper can be killed.
type boundary struct {
	// name is the barrier's own name, as the protocol spells it.
	name string
	// nth is which arrival to kill at; mount-made fires once per nested
	// mount and the rest fire once.
	nth int
	// what the machine is holding when the kill lands, for the reader of
	// a failure.
	holding string
}

func run() int {
	env := flag.String("env", filepath.Join(home(), "campcheck"),
		"the environment root of a scratch composition")
	camp := flag.String("camp", filepath.Join(home(), "campcheck", "camp-barriers"),
		"a camp built with -tags camptest")
	only := flag.String("at", "", "measure only this boundary")
	flag.Parse()

	if os.Geteuid() == 0 {
		fmt.Fprintln(os.Stderr, "Run this as yourself. camp's front end refuses "+
			"to run as root and this drives the front end; camp elevates for the "+
			"one step that needs it.")
		return 2
	}
	if _, err := os.Stat(*camp); err != nil {
		fmt.Fprintf(os.Stderr, "%v\nBuild it: go build -tags camptest -o %s ./cmd/camp\n",
			err, *camp)
		return 2
	}

	if err := warmSudo(*env); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 2
	}

	fixture, err := open(*env, *camp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 2
	}
	defer fixture.restoreConfiguration()

	fmt.Printf("environment: %s\ncamp:        %s\nrecords:     %s\n\n",
		*env, *camp, probe.StateDir())

	// The census. One ordinary run, taken apart again, which establishes
	// three things nothing else can: that this composition works at all,
	// how many nested mounts it has -- the barrier fires once per mount and
	// the matrix needs every one of them -- and the hash of the tree camp
	// promises never to write into.
	fmt.Println("-- census: one ordinary run, to have something to compare to")
	mounts, err := fixture.census()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nthe composition does not come up and down "+
			"cleanly, so nothing after this would mean anything:\n%v\n", err)
		return 2
	}
	fmt.Printf("   %d mounts, hash %s\n\n", mounts, fixture.hash)

	order := []boundary{{name: "staging-bound", nth: 1,
		holding: "the staging self-bind and nothing inside it"}}
	for nested := 1; nested <= mounts; nested++ {
		order = append(order, boundary{name: "mount-made", nth: nested,
			holding: fmt.Sprintf("the staging self-bind and %d nested mount(s)", nested)})
	}
	order = append(order,
		boundary{name: "staging-verified", nth: 1,
			holding: "the whole tree, built and checked, still in staging"},
		boundary{name: "live-bound", nth: 1,
			holding: "the tree in staging and the live self-bind as well"},
		boundary{name: "moved", nth: 1,
			holding: "the tree at the live path, and the staging self-bind still there"},
		boundary{name: "staging-unbound", nth: 1,
			holding: "the composition, complete, with no reply written"},
		boundary{name: "reply-encoded", nth: 1,
			holding: "the composition, complete, with the reply built and unsent"},
		boundary{name: "reply-received", nth: 1,
			holding: "the composition, complete, with the front end holding the reply"},
	)

	verdict := &probe.Verdict{}
	fmt.Println("-- the matrix")
	for _, at := range order {
		if *only != "" && at.name != *only {
			continue
		}
		one := verdict.Case(at.label())
		fixture.measure(one, at)
		one.Done()
		if !one.Held() && !fixture.clean() {
			one.Stop("the machine is not clean after this case, so the rest " +
				"would be measuring what this one left")
			break
		}
	}

	return verdict.Print("camp recovers from the record alone at every kill boundary.")
}

func (b boundary) label() string {
	if b.name == "mount-made" {
		return fmt.Sprintf("%s #%d", b.name, b.nth)
	}
	return b.name
}

// fixture is the scratch composition and what the driver knows about it.
type fixture struct {
	env  string
	camp string
	hash string
	work string
	live string
}

func open(env, camp string) (*fixture, error) {
	f := &fixture{env: env, camp: camp}
	if _, err := os.Stat(f.configuration()); err != nil {
		if _, away := os.Stat(f.configuration() + ".away"); away == nil {
			if err := os.Rename(f.configuration()+".away", f.configuration()); err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("%s has no configuration", env)
		}
	}
	if _, found, err := probe.RecordFor(env); err != nil {
		return nil, err
	} else if found {
		return nil, fmt.Errorf("%s already has a record. This driver takes a "+
			"composition up and down many times and has to start from nothing: "+
			"run 'camp down' there first", env)
	}
	table, err := probe.Table()
	if err != nil {
		return nil, err
	}
	if standing := probe.Under(table, env); len(standing) > 0 {
		return nil, fmt.Errorf("%s already has mounts under it: %s",
			env, probe.Points(standing))
	}
	return f, nil
}

func (f *fixture) configuration() string {
	return filepath.Join(f.env, ".camp", "config.yml")
}

// trees are what camp promises never to write into, plus the store it
// promises never to remove.
func (f *fixture) hashes() (map[string]string, error) {
	found := map[string]string{}
	for _, tree := range []string{"workspace", "code", ".camp/storage"} {
		hash, err := probe.Tree(filepath.Join(f.env, filepath.FromSlash(tree)))
		if err != nil {
			return nil, err
		}
		found[tree] = hash
	}
	return found, nil
}

// census runs the composition once, ordinarily, and learns from it.
func (f *fixture) census() (int, error) {
	if up := probe.Run(f.env, f.camp, "up"); up.Code != 0 {
		return 0, fmt.Errorf("'camp up' exited %d:\n%s%s", up.Code, up.Out, up.Err)
	}
	record, found, err := probe.RecordFor(f.env)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("'camp up' succeeded and left no record")
	}
	f.hash, f.live = record.Hash, record.Live
	f.work = filepath.Join(f.env, ".camp", "work", record.Hash)
	mounts := len(record.Mounts)

	if down := probe.Run(f.env, f.camp, "down"); down.Code != 0 {
		return 0, fmt.Errorf("'camp down' exited %d:\n%s%s", down.Code, down.Out, down.Err)
	}
	if !f.clean() {
		return 0, fmt.Errorf("something is still mounted under %s after an "+
			"ordinary 'camp down'", f.env)
	}
	return mounts, nil
}

// camps reports whether a path is one of camp's own places: inside the
// environment, or inside the directory the helper takes a mount to before
// it unmounts it.
func (f *fixture) camps(path string) bool {
	for _, base := range []string{f.env, "/run/camp"} {
		if path == base || strings.HasPrefix(path, base+"/") {
			return true
		}
	}
	return false
}

// clean reports whether nothing of this composition is mounted.
func (f *fixture) clean() bool {
	table, err := probe.Table()
	if err != nil {
		return false
	}
	return len(probe.Under(table, f.env)) == 0
}

// measure is one boundary, from a clean machine to a clean machine.
func (f *fixture) measure(one *probe.Case, at boundary) {
	before, err := probe.Table()
	if err != nil {
		one.Stop("the mount table could not be read: %v", err)
		return
	}
	// Per case, and not against the first run: what has to hold is that
	// this kill and this recovery changed nothing, and a comparison against
	// a baseline further back would blame one case for what an earlier one
	// did.
	was, err := f.hashes()
	if err != nil {
		one.Stop("the trees could not be hashed: %v", err)
		return
	}

	one.Note = fmt.Sprintf("the kill lands while the machine is holding %s",
		at.holding)

	killed, reached, err := f.upKilledAt(at)
	if err != nil {
		one.Stop("'camp up' could not be run: %v", err)
		return
	}
	if reached < at.nth {
		// Not a pass and not a failure: the boundary was never reached, so
		// nothing about it was measured. What the run did leave behind still
		// has to go, or the next case would start from it.
		one.Skip("the run never stopped at this boundary (%d of %d arrivals "+
			"seen). Either this camp was not built with -tags camptest, or the "+
			"release of an earlier arrival was seen too late to stop at this one",
			reached, at.nth)
		f.cleanup()
		return
	}
	one.Require(killed.Code != 0,
		"'camp up' exited 0 after its helper was killed at %s", at.name)

	// The record, before anything is taken down and with the configuration
	// gone. This is the whole claim: what is on the machine can be
	// described and removed from this file alone.
	f.awayConfiguration(one)
	defer f.restoreConfiguration()

	record, found, err := probe.RecordFor(f.env)
	switch {
	case err != nil:
		one.Stop("the record could not be read: %v", err)
		return
	case !found:
		one.Stop("the run was killed at %s and no record names this "+
			"composition. The record is written before the helper is invoked, "+
			"so a machine holding %s has nothing to be recovered from",
			at.name, at.holding)
		return
	}
	one.Require(record.Phase == "mounting" || record.Phase == "up" ||
		record.Phase == "partial",
		"the record's phase is %q, and a run killed at %s left a machine that "+
			"may be holding %s", record.Phase, at.name, at.holding)

	// "Describes the composition" is not a phrase to search for. What it
	// means is that a person with no configuration can see, from this
	// output alone, which composition this is and what of it is standing --
	// so that is what is required: the tree's own path, the record's name,
	// and every mount that is actually on the machine, at the place it is
	// actually at. camp wraps its lines at spaces and never inside a path,
	// so a path either appears whole or does not appear.
	// The exit code is deliberately not asserted. A composition that is
	// partly up is exactly when 'camp status' has to exit non-zero -- it
	// says "run camp down to take it apart", and that is the true answer
	// after every kill in this matrix. What is required is what it says.
	status := probe.Run(f.env, f.camp, "status")
	said := status.Out + status.Err
	one.Require(strings.Contains(said, f.live),
		"'camp status' with no configuration does not name %s:\n%s", f.live, said)
	one.Require(strings.Contains(said, record.Hash),
		"'camp status' with no configuration does not name the record %s:\n%s",
		record.Hash, said)

	standing, err := probe.Table()
	if err != nil {
		one.Stop("the mount table could not be read: %v", err)
		return
	}
	for _, mount := range probe.Under(standing, f.env) {
		one.Require(strings.Contains(said, mount.Point),
			"%s is mounted and 'camp status' with no configuration does not "+
				"name it:\n%s", mount.Point, said)
	}

	f.recover(one, before, was)
}

// recover takes the composition down from the record and holds the result
// to the review's five requirements.
func (f *fixture) recover(one *probe.Case, before map[int]probe.Mount,
	was map[string]string) {
	down := probe.Run(f.env, f.camp, "down")

	after, err := probe.Table()
	if err != nil {
		one.Stop("the mount table could not be read: %v", err)
		return
	}
	// Only camp's own places. A mount that appeared elsewhere on a machine
	// somebody else is also working on is not this measurement's business,
	// and counting it would make the driver fail for somebody else's work.
	var survived []probe.Mount
	for _, mount := range probe.Arrived(before, after) {
		if f.camps(mount.Point) {
			survived = append(survived, mount)
		}
	}
	one.Require(len(survived) == 0,
		"mounts camp made are still standing: %s", probe.Points(survived))

	// This one is not restricted, and must not be: the failure it exists to
	// catch is root unmounting something that was never camp's.
	gone := probe.Gone(before, after)
	one.Require(len(gone) == 0,
		"mounts that were nothing to do with camp disappeared: %s", probe.Points(gone))

	// Every path still mounted has to be named by what camp said, and by
	// the record it kept. A teardown that cannot remove something and does
	// not say which thing leaves a person with nothing to act on.
	standing := probe.Under(after, f.env)
	for _, mount := range standing {
		one.Require(down.Says(mount.Point),
			"%s is still mounted and 'camp down' did not name it:\n%s%s",
			mount.Point, down.Out, down.Err)
	}

	record, found, err := probe.RecordFor(f.env)
	if err != nil {
		one.Stop("the record could not be read: %v", err)
		return
	}
	switch {
	case len(standing) > 0:
		one.Require(found, "%d mount(s) are still standing and the record is "+
			"gone: %s", len(standing), probe.Points(standing))
		if found {
			for _, mount := range standing {
				one.Require(names(record.Places(), mount.Point),
					"%s is still mounted and the record does not name it as a "+
						"place to look", mount.Point)
			}
		}
		one.Require(down.Code != 0,
			"'camp down' exited 0 with %d mount(s) still standing", len(standing))
	default:
		one.Require(!found,
			"everything came down and the record %s was kept", record.Path)
		one.Require(down.Code == 0,
			"nothing is mounted and 'camp down' exited %d:\n%s%s",
			down.Code, down.Out, down.Err)
	}

	// And the invariant that outranks all of it: camp composes, it does not
	// write into what it composes.
	now, err := f.hashes()
	if err != nil {
		one.Stop("the trees could not be hashed: %v", err)
		return
	}
	for tree, before := range was {
		one.Require(now[tree] == before,
			"%s is not what it was before this case: %s became %s",
			tree, short(before), short(now[tree]))
	}

	// A case that could not clean up hands the next one a machine that is
	// not the one it thinks it is measuring, so the record goes whatever
	// happened -- and the failures above have already been recorded.
	if found {
		probe.Run(f.env, f.camp, "forget", record.Hash)
	}
}

// upKilledAt arms the barrier and runs 'camp up' into it.
//
// The barrier carries one name and one mode, and mount-made fires at every
// nested mount, so stopping at the nth of them is done by waiting at the
// n-1 before it: each is released in turn, and the arming file is rewritten
// to "kill" before the last release, so the helper reads "kill" when it
// arrives at the one that matters. Nothing about the barrier protocol
// changes for this; it re-reads the file at every call, which is what makes
// it possible.
func (f *fixture) upKilledAt(at boundary) (probe.Result, int, error) {
	if err := os.MkdirAll(f.work, 0o755); err != nil {
		return probe.Result{}, 0, err
	}
	arm := filepath.Join(f.work, "camp-barrier")
	release := filepath.Join(f.work, "camp-barrier.go")
	os.Remove(release)

	mode := "wait"
	if at.nth == 1 {
		mode = "kill"
	}
	if err := os.WriteFile(arm, []byte(at.name+" "+mode+"\n"), 0o644); err != nil {
		return probe.Result{}, 0, err
	}
	defer os.Remove(arm)
	defer os.Remove(release)

	command := exec.Command(f.camp, "up")
	command.Dir = f.env
	command.Stdin = os.Stdin
	var out strings.Builder
	command.Stdout = &out
	pipe, err := command.StderrPipe()
	if err != nil {
		return probe.Result{}, 0, err
	}
	if err := command.Start(); err != nil {
		return probe.Result{}, 0, err
	}

	reached := 0
	var errors strings.Builder
	lines := bufio.NewScanner(pipe)
	for lines.Scan() {
		line := lines.Text()
		errors.WriteString(line + "\n")
		switch {
		case strings.Contains(line, "camp barrier: reached "+at.name+" (wait)"):
			reached++
			if reached == at.nth-1 {
				// The rearm before the release, so that the helper's next read
				// of the file is the one that kills it.
				os.WriteFile(arm, []byte(at.name+" kill\n"), 0o644)
			}
			os.WriteFile(release, nil, 0o644)
		case strings.Contains(line, "camp barrier: released "+at.name):
			// Taken away again, or the next arrival would let itself through.
			// If the helper gets there first the run does not stop where it
			// was meant to, and the case reports itself as not measured
			// rather than as held.
			os.Remove(release)
		case strings.Contains(line, "camp barrier: reached "+at.name+" (kill)"):
			reached++
		}
	}
	_, _ = io.Copy(io.Discard, pipe)

	var result probe.Result
	result.Code, result.Failed = probe.Ended(command.Wait())
	result.Out, result.Err = out.String(), errors.String()
	return result, reached, nil
}

// cleanup puts the machine back without measuring anything, for the cases
// that never got as far as a measurement.
func (f *fixture) cleanup() {
	if _, found, err := probe.RecordFor(f.env); err == nil && found {
		probe.Run(f.env, f.camp, "down")
	}
	if record, found, err := probe.RecordFor(f.env); err == nil && found {
		probe.Run(f.env, f.camp, "forget", record.Hash)
	}
}

func (f *fixture) awayConfiguration(one *probe.Case) {
	if err := os.Rename(f.configuration(), f.configuration()+".away"); err != nil {
		one.Stop("the configuration could not be moved out of the way: %v", err)
	}
}

func (f *fixture) restoreConfiguration() {
	if _, err := os.Stat(f.configuration()); err == nil {
		return
	}
	os.Rename(f.configuration()+".away", f.configuration())
}

func names(places []string, path string) bool {
	for _, place := range places {
		if place == path {
			return true
		}
	}
	return false
}

func short(hash string) string {
	if len(hash) < 12 {
		return "(nothing there)"
	}
	return hash[:12]
}

func home() string {
	directory, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return directory
}

// warmSudo makes sure the privileged steps will not stop to ask for a
// password in the middle of a measurement.
//
// Two machines to satisfy. On a person's own machine sudo asks on a
// terminal and cannot ask anywhere else -- C33 -- so the password is taken
// once, here, before anything is measured. On a machine that exists for
// one run there is no password and no terminal either, and asking for one
// would fail on a machine where nothing needed asking. So: find out
// whether anything needs asking, and only then ask.
func warmSudo(dir string) error {
	if probe.Run(dir, "sudo", "-n", "true").Code == 0 {
		return nil
	}
	if result := probe.Run(dir, "sudo", "-v"); result.Code != 0 {
		return fmt.Errorf("sudo would not warm up: %s%s", result.Out, result.Err)
	}
	return nil
}
