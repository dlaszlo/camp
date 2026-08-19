// Command renamerace swaps the environment's name under the privileged
// helper at each of its resolutions, and requires that nothing outside the
// environment changes.
//
// This is the measurement the review asked for by name, and the assertion
// is deliberately not "camp errors". The invoking user owns the
// environment root and normally its parent, so they can rename it away and
// leave a symbolic link at the old name at any instant they choose. camp
// is allowed to notice and refuse, and it is allowed to carry on: what it
// may never do is act at the link's target. The helper is root, and a root
// process that binds, moves, remounts or unmounts wherever a name happens
// to point is a confused deputy -- somebody who can edit a configuration
// would have reached root through it.
//
// So the trap tree is what is measured, not camp's exit code:
//
//   - no mount id outside the original environment changes;
//   - no mount attribute outside it changes;
//   - no inode mode outside it changes;
//   - the trap tree hashes identically before and after.
//
// The four resolutions it swaps at, and what each one is about:
//
//	base-owned         the helper has opened and checked its base
//	prechecked         every operand compared, nothing mounted yet
//	before-move-open   before the move's two descriptors are opened
//	stands-there       teardown: identity checked, before the unmount
//
// # Running it
//
// It needs a real terminal for sudo's prompt and a camp built with the
// barriers:
//
//	go build -tags camptest -o ~/campcheck/camp-barriers ./cmd/camp
//	go run ./renamerace -env ~/campcheck -camp ~/campcheck/camp-barriers
//
// Run it as yourself. It uses sudo for the two things that need root: the
// trap tree, which has to be root-owned and full of mounts nothing else
// on the machine has, and taking that tree down again afterwards.
//
// It renames a real environment and makes real mounts. Point it at a
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

// swap is one resolution the environment's name is replaced at.
type swap struct {
	// barrier is the name the protocol spells.
	barrier string
	// command is what has to be running for that barrier to be reached.
	command string
	// about is what the helper is doing when the name goes.
	about string
}

var swaps = []swap{
	{barrier: "base-owned", command: "up",
		about: "the base is open and checked, and nothing else has been resolved"},
	{barrier: "prechecked", command: "up",
		about: "every operand has been compared and nothing is mounted"},
	{barrier: "before-move-open", command: "up",
		about: "the tree is built and verified, and the move's descriptors are not open yet"},
	{barrier: "stands-there", command: "down",
		about: "a mount's identity has been checked and it is about to come down"},
}

func run() int {
	env := flag.String("env", filepath.Join(home(), "campcheck"),
		"the environment root of a scratch composition")
	camp := flag.String("camp", filepath.Join(home(), "campcheck", "camp-barriers"),
		"a camp built with -tags camptest")
	trap := flag.String("trap", "/var/tmp/camp-trap",
		"where to build the root-owned trap tree")
	only := flag.String("at", "", "measure only this barrier")
	flag.Parse()

	if os.Geteuid() == 0 {
		fmt.Fprintln(os.Stderr, "Run this as yourself. The swap this measures "+
			"is one the invoking user can make, and camp's front end refuses to "+
			"run as root anyway.")
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

	fixture := &fixture{env: *env, camp: *camp, trap: *trap}
	if err := fixture.check(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 2
	}

	fmt.Printf("environment: %s\ncamp:        %s\ntrap:        %s\n\n",
		*env, *camp, *trap)

	// The hash, from one ordinary run. The barrier is armed by writing
	// camp's work directory for this composition, and the work directory's
	// name is the hash -- so the driver has to know it before it can arm
	// anything, and the honest way to learn it is to let camp write it.
	fmt.Println("-- census: one ordinary run, to learn this composition's name")
	if err := fixture.census(); err != nil {
		fmt.Fprintf(os.Stderr, "\nthe composition does not come up and down "+
			"cleanly, so nothing after this would mean anything:\n%v\n", err)
		return 2
	}
	fmt.Printf("   hash %s, live %s\n\n", fixture.hash, fixture.live)

	fmt.Println("-- the trap tree")
	if err := fixture.buildTrap(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 2
	}
	defer fixture.removeTrap()
	fmt.Printf("   %s, root-owned, with %d mount(s) of its own\n\n",
		fixture.trap, len(fixture.trapMounts()))

	verdict := &probe.Verdict{}
	fmt.Println("-- the swaps")
	for _, at := range swaps {
		if *only != "" && at.barrier != *only {
			continue
		}
		one := verdict.Case(at.barrier)
		fixture.measure(one, at)
		one.Done()

		// Cleaning up is not one of the three things this case asserts, and
		// folding it into them would report a swap that changed nothing
		// outside the environment as a swap that failed. It is its own
		// finding, in its own case, with what is standing named -- and the
		// run stops there, because the next swap would be measuring what
		// this one left.
		if left := fixture.settle(); left != "" {
			after := verdict.Case("after " + at.barrier)
			after.Stop("%s", left)
			after.Done()
			break
		}
	}

	return verdict.Print("A rename of the environment at any resolution " +
		"changes nothing outside it.")
}

type fixture struct {
	env  string
	camp string
	trap string
	hash string
	live string
}

func (f *fixture) check() error {
	if _, err := os.Stat(filepath.Join(f.env, ".camp", "config.yml")); err != nil {
		away := filepath.Join(f.env, ".camp", "config.yml.away")
		if _, err := os.Stat(away); err != nil {
			return fmt.Errorf("%s has no configuration", f.env)
		}
		if err := os.Rename(away, filepath.Join(f.env, ".camp", "config.yml")); err != nil {
			return err
		}
	}
	if _, found, err := probe.RecordFor(f.env); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%s already has a record: run 'camp down' there first", f.env)
	}
	if _, err := os.Lstat(f.env + ".real"); err == nil {
		return fmt.Errorf("%s.real is in the way. A previous run was "+
			"interrupted between the rename and putting the name back; move it "+
			"into place by hand before running this again", f.env)
	}
	return nil
}

func (f *fixture) census() error {
	if up := probe.Run(f.env, f.camp, "up"); up.Code != 0 {
		return fmt.Errorf("'camp up' exited %d:\n%s%s", up.Code, up.Out, up.Err)
	}
	record, found, err := probe.RecordFor(f.env)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("'camp up' succeeded and left no record")
	}
	f.hash, f.live = record.Hash, record.Live
	if down := probe.Run(f.env, f.camp, "down"); down.Code != 0 {
		return fmt.Errorf("'camp down' exited %d:\n%s%s", down.Code, down.Out, down.Err)
	}
	return nil
}

// work is camp's work directory for this composition, where the barrier is
// armed. The second form is the same path once the environment's name is a
// link to the trap tree, which is where the release has to be written: the
// barrier holds that path as a string and resolves it again, which is the
// very thing being measured.
func (f *fixture) work() string {
	return filepath.Join(f.env, ".camp", "work", f.hash)
}

func (f *fixture) trapWork() string {
	return filepath.Join(f.trap, ".camp", "work", f.hash)
}

// buildTrap makes the tree the swapped name points at.
//
// Root-owned, so that a helper acting through the link would be acting
// where the invoking user could not have prepared anything themselves, and
// laid out like the environment so that every path the helper might still
// be carrying resolves to something. Its mounts are what make a change
// visible: a bind of a directory nothing else on the machine has, at each
// of the two places camp mounts.
//
// One directory in it belongs to the driver rather than to root: the work
// directory, where the barrier's release file goes. That is the driver's
// own channel and not camp's -- the barrier resolves that path by name
// like everything else, so after the swap the release has to be written on
// this side of the link or the helper would wait out its minute.
func (f *fixture) buildTrap() error {
	f.removeTrap()

	directories := []string{
		f.trap,
		filepath.Join(f.trap, "code"),
		filepath.Join(f.trap, "workspace"),
		filepath.Join(f.trap, "live"),
		filepath.Join(f.trap, "marker-live"),
		filepath.Join(f.trap, "marker-staging"),
		filepath.Join(f.trap, ".camp", "storage"),
		filepath.Join(f.trap, ".camp", "work", f.hash, "staging"),
	}
	if out, err := sudo(append([]string{"mkdir", "-p"}, directories...)...); err != nil {
		return fmt.Errorf("making the trap tree: %v\n%s", err, out)
	}
	for name, content := range map[string]string{
		"code/TRAP":           "this file is in the trap tree and nothing may write to it\n",
		"workspace/TRAP":      "this file is in the trap tree and nothing may write to it\n",
		"marker-live/TRAP":    "the mount at the trap tree's live path\n",
		"marker-staging/TRAP": "the mount at the trap tree's staging path\n",
		".camp/storage/TRAP":  "the trap tree's storage\n",
	} {
		if out, err := sudoWrite(filepath.Join(f.trap, filepath.FromSlash(name)), content); err != nil {
			return fmt.Errorf("filling the trap tree: %v\n%s", err, out)
		}
	}

	// Two mounts nothing else on this machine has, at the two places camp
	// mounts: if a helper resolving a swapped name acts at all, it acts on
	// one of these, and their ids are what says so.
	for _, bind := range [][2]string{
		{filepath.Join(f.trap, "marker-live"), filepath.Join(f.trap, "live")},
		{filepath.Join(f.trap, "marker-staging"),
			filepath.Join(f.trap, ".camp", "work", f.hash, "staging")},
	} {
		if out, err := sudo("mount", "--bind", "-o", "ro", bind[0], bind[1]); err != nil {
			return fmt.Errorf("mounting the trap tree: %v\n%s", err, out)
		}
	}

	// And the one directory the driver owns, for the reason above.
	if out, err := sudo("chown", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		f.trapWork()); err != nil {
		return fmt.Errorf("giving the driver its release channel: %v\n%s", err, out)
	}
	return nil
}

func (f *fixture) removeTrap() {
	table, err := probe.Table()
	if err == nil {
		for _, mount := range probe.Under(table, f.trap) {
			sudo("umount", mount.Point)
		}
	}
	sudo("rm", "-rf", f.trap)
}

func (f *fixture) trapMounts() []probe.Mount {
	table, err := probe.Table()
	if err != nil {
		return nil
	}
	return probe.Under(table, f.trap)
}

// measure is one swap, from a clean machine to a clean machine.
func (f *fixture) measure(one *probe.Case, at swap) {
	// A teardown can only be interrupted if there is something to tear
	// down.
	if at.command == "down" {
		if up := probe.Run(f.env, f.camp, "up"); up.Code != 0 {
			one.Stop("'camp up' exited %d, so there was no teardown to "+
				"interrupt:\n%s%s", up.Code, up.Out, up.Err)
			return
		}
	}

	before, err := probe.Table()
	if err != nil {
		one.Stop("the mount table could not be read: %v", err)
		return
	}
	trapWas, err := probe.Tree(f.trap)
	if err != nil {
		one.Stop("the trap tree could not be hashed: %v", err)
		return
	}
	modesWere, err := probe.Modes(f.trap)
	if err != nil {
		one.Stop("the trap tree's modes could not be read: %v", err)
		return
	}

	result, swapped, err := f.runSwapped(at)
	if err != nil {
		one.Stop("'camp %s' could not be run: %v", at.command, err)
		return
	}
	if !swapped {
		one.Skip("the run never stopped at %s, so the name was never swapped "+
			"under it. Either this camp was not built with -tags camptest, or "+
			"this command does not reach that barrier", at.barrier)
		return
	}
	one.Note = fmt.Sprintf("the name was swapped while %s; 'camp %s' exited %d",
		at.about, at.command, result.Code)

	after, err := probe.Table()
	if err != nil {
		one.Stop("the mount table could not be read: %v", err)
		return
	}

	// The whole assertion, in three parts, and none of them is about what
	// camp said.
	for _, mount := range probe.Arrived(before, after) {
		if f.inside(mount.Point) {
			continue
		}
		one.Require(false, "a mount appeared outside %s while the name was "+
			"swapped:\n      %s", f.env, mount.Line)
	}
	for _, mount := range probe.Gone(before, after) {
		if f.inside(mount.Point) {
			continue
		}
		one.Require(false, "a mount outside %s disappeared while the name was "+
			"swapped:\n      %s", f.env, mount.Line)
	}
	for id, was := range before {
		if f.inside(was.Point) {
			continue
		}
		now, still := after[id]
		if !still {
			continue // already reported above
		}
		one.Require(now.Line == was.Line,
			"a mount outside %s changed while the name was swapped:\n"+
				"      was: %s\n      now: %s", f.env, was.Line, now.Line)
	}

	trapNow, err := probe.Tree(f.trap)
	if err != nil {
		one.Stop("the trap tree could not be hashed: %v", err)
		return
	}
	one.Require(trapNow == trapWas,
		"the trap tree's content changed: %s became %s", short(trapWas), short(trapNow))

	modesNow, err := probe.Modes(f.trap)
	if err != nil {
		one.Stop("the trap tree's modes could not be read: %v", err)
		return
	}
	for path, was := range modesWere {
		now, still := modesNow[path]
		one.Require(still, "%s is gone from the trap tree", path)
		if still {
			one.Require(now == was, "%s changed mode in the trap tree: %v became %v",
				path, was, now)
		}
	}
	for path := range modesNow {
		if _, had := modesWere[path]; !had {
			one.Require(false, "%s appeared in the trap tree", path)
		}
	}
}

// inside reports whether a path belongs to the environment, under either
// of the two names it has while a swap is in progress.
func (f *fixture) inside(path string) bool {
	for _, base := range []string{f.env, f.env + ".real"} {
		if path == base || strings.HasPrefix(path, base+"/") {
			return true
		}
	}
	return false
}

// runSwapped runs camp, waits for the barrier to announce itself, replaces
// the environment's name with a link to the trap tree, and lets it go on.
func (f *fixture) runSwapped(at swap) (probe.Result, bool, error) {
	if err := os.MkdirAll(f.work(), 0o755); err != nil {
		return probe.Result{}, false, err
	}
	arm := filepath.Join(f.work(), "camp-barrier")
	if err := os.WriteFile(arm, []byte(at.barrier+" wait\n"), 0o644); err != nil {
		return probe.Result{}, false, err
	}
	defer os.Remove(arm)
	os.Remove(filepath.Join(f.work(), "camp-barrier.go"))
	os.Remove(filepath.Join(f.trapWork(), "camp-barrier.go"))

	command := exec.Command(f.camp, at.command)
	// From inside the environment, the way a person runs it. The process
	// keeps the directory it started in as a handle, so the rename does not
	// move it -- and for the teardown the record was already chosen before
	// the barrier that swaps the name.
	command.Dir = f.env
	command.Stdin = os.Stdin
	var out strings.Builder
	command.Stdout = &out
	pipe, err := command.StderrPipe()
	if err != nil {
		return probe.Result{}, false, err
	}
	if err := command.Start(); err != nil {
		return probe.Result{}, false, err
	}

	swapped := false
	var errors strings.Builder
	lines := bufio.NewScanner(pipe)
	for lines.Scan() {
		line := lines.Text()
		errors.WriteString(line + "\n")
		if !strings.Contains(line, "camp barrier: reached "+at.barrier+" (wait)") {
			continue
		}
		if swapped {
			continue // it fires once per target; the first one is the swap
		}
		// The swap itself: the environment goes somewhere else and a link
		// to the trap tree takes its name. Both are ordinary operations for
		// whoever owns the directory, which is the person this defends
		// against.
		if err := os.Rename(f.env, f.env+".real"); err != nil {
			break
		}
		if err := os.Symlink(f.trap, f.env); err != nil {
			os.Rename(f.env+".real", f.env)
			break
		}
		swapped = true
		// Into the trap, because that is where the helper will look: it
		// holds the work directory as a string built from the base it was
		// given, and resolves it again at every barrier.
		os.WriteFile(filepath.Join(f.trapWork(), "camp-barrier.go"), nil, 0o644)
	}
	_, _ = io.Copy(io.Discard, pipe)

	var result probe.Result
	result.Code, result.Failed = probe.Ended(command.Wait())
	result.Out, result.Err = out.String(), errors.String()

	f.putTheNameBack()
	return result, swapped, nil
}

// putTheNameBack undoes the swap, and is safe to call when there was none.
func (f *fixture) putTheNameBack() {
	if target, err := os.Readlink(f.env); err == nil && target == f.trap {
		os.Remove(f.env)
	}
	if _, err := os.Lstat(f.env + ".real"); err == nil {
		os.Rename(f.env+".real", f.env)
	}
	os.Remove(filepath.Join(f.trapWork(), "camp-barrier.go"))
}

// settle brings the composition down again, whatever the swap left, and
// says what is still standing.
//
// The empty string means the environment is clean. Anything else is a
// sentence naming what is on the machine and whether camp still has a
// record for it, because those two together are what decides whether a
// person can get out of it with 'camp down' or has to unmount by hand.
func (f *fixture) settle() string {
	f.putTheNameBack()
	if _, found, err := probe.RecordFor(f.env); err == nil && found {
		probe.Run(f.env, f.camp, "down")
	}
	record, found, err := probe.RecordFor(f.env)
	if err != nil {
		return fmt.Sprintf("the record could not be read: %v", err)
	}
	if found {
		probe.Run(f.env, f.camp, "forget", record.Hash)
	}

	table, err := probe.Table()
	if err != nil {
		return fmt.Sprintf("the mount table could not be read: %v", err)
	}
	standing := probe.Under(table, f.env)
	if len(standing) == 0 {
		return ""
	}
	_, still, _ := probe.RecordFor(f.env)
	if still {
		return fmt.Sprintf("%d mount(s) are still standing after 'camp down': "+
			"%s. The record is still there, so camp can be asked again",
			len(standing), probe.Points(standing))
	}
	return fmt.Sprintf("%d mount(s) are still standing and camp has no record "+
		"of them: %s.\n      Nothing camp can be told will remove these -- a "+
		"teardown is built from the record, and the record is gone. They have "+
		"to be unmounted by hand", len(standing), probe.Points(standing))
}

func sudo(args ...string) (string, error) {
	result := probe.Run("/", "sudo", args...)
	if result.Failed != nil {
		return result.Out + result.Err, result.Failed
	}
	if result.Code != 0 {
		return result.Out + result.Err, fmt.Errorf("sudo %s exited %d",
			strings.Join(args, " "), result.Code)
	}
	return result.Out + result.Err, nil
}

// sudoWrite puts a file in the trap tree as root, through tee, because
// only root may write there and that is the point of the tree.
func sudoWrite(path, content string) (string, error) {
	command := exec.Command("sudo", "tee", path)
	command.Stdin = strings.NewReader(content)
	out, err := command.CombinedOutput()
	return string(out), err
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
