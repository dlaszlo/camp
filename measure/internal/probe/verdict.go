package probe

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// A driver prints a verdict, not a log.
//
// These are run by hand, at a terminal, by somebody who wants to know
// whether camp survives a thing -- not somebody who wants to read what
// camp did. So each case prints one line while it runs, and everything
// that failed is printed again at the end with what was seen and what was
// required, in the order it happened. A run that passes is a screen of
// "ok" and one sentence; a run that fails names exactly what is standing.
type Verdict struct {
	cases []*Case
}

// Case is one boundary, one swap -- one thing that either held or did not.
type Case struct {
	Name string
	// Note is what this case is about, printed only when something in it
	// failed: a reader of a passing line does not need it, and a reader of
	// a failing one needs to know what the machine was doing.
	Note    string
	failed  []string
	skipped string
}

// Case starts one and prints nothing yet.
func (v *Verdict) Case(name string) *Case {
	one := &Case{Name: name}
	v.cases = append(v.cases, one)
	return one
}

// Require records a failure when the condition does not hold.
//
// The message says what was required and what was seen, because a
// verdict that only says "failed" sends the reader back to do the
// measurement again by hand.
func (c *Case) Require(held bool, format string, args ...any) {
	if !held {
		c.failed = append(c.failed, fmt.Sprintf(format, args...))
	}
}

// Stop records a failure that ended the case: the rest was not measured.
func (c *Case) Stop(format string, args ...any) {
	c.failed = append(c.failed, fmt.Sprintf(format, args...))
	c.skipped = "the rest of this case was not measured"
}

// Skip says this case did not run, and why. It is neither a pass nor a
// failure: a driver that counted an unrun case as a pass would be a
// driver that passes by not running.
func (c *Case) Skip(format string, args ...any) {
	c.skipped = fmt.Sprintf(format, args...)
}

// Held reports whether nothing has failed in this case so far.
func (c *Case) Held() bool { return len(c.failed) == 0 }

// Done prints the one line this case gets while the run is happening.
func (c *Case) Done() {
	switch {
	case c.skipped != "" && len(c.failed) == 0:
		fmt.Printf("  %-28s skipped -- %s\n", c.Name, c.skipped)
	case len(c.failed) == 0:
		fmt.Printf("  %-28s ok\n", c.Name)
	default:
		fmt.Printf("  %-28s FAILED (%d)\n", c.Name, len(c.failed))
	}
}

// Print writes the verdict and returns the exit code the driver should
// leave with.
func (v *Verdict) Print(what string) int {
	passed, failed, skipped := 0, 0, 0
	for _, one := range v.cases {
		switch {
		case len(one.failed) > 0:
			failed++
		case one.skipped != "":
			skipped++
		default:
			passed++
		}
	}

	fmt.Printf("\n-- verdict ----------------------------------------------\n")
	fmt.Printf("  %s\n", what)
	fmt.Printf("  %d held, %d failed, %d not measured\n", passed, failed, skipped)

	if failed == 0 && skipped == 0 {
		fmt.Printf("\n  Every case held.\n")
		return 0
	}
	for _, one := range v.cases {
		if len(one.failed) == 0 {
			continue
		}
		fmt.Printf("\n  %s\n", one.Name)
		if one.Note != "" {
			fmt.Printf("    %s\n", one.Note)
		}
		for _, why := range one.failed {
			fmt.Printf("    - %s\n", why)
		}
		if one.skipped != "" {
			fmt.Printf("    (%s)\n", one.skipped)
		}
	}
	if skipped > 0 {
		fmt.Printf("\n  Not measured:\n")
		for _, one := range v.cases {
			if len(one.failed) == 0 && one.skipped != "" {
				fmt.Printf("    - %s: %s\n", one.Name, one.skipped)
			}
		}
	}
	if failed > 0 {
		return 1
	}
	return 2
}

// Result is what a command did.
type Result struct {
	Code   int
	Out    string
	Err    string
	Failed error
}

// Says reports whether either stream contains a string.
func (r Result) Says(text string) bool {
	return strings.Contains(r.Out, text) || strings.Contains(r.Err, text)
}

// Run executes a command and captures both streams.
//
// Stdin is the terminal, deliberately: sudo asks for a password there and
// cannot ask anywhere else -- measured, C33 -- so a driver that closed it
// would turn every privileged step into a failure that says nothing.
func Run(dir string, name string, args ...string) Result {
	command := exec.Command(name, args...)
	command.Dir = dir
	command.Stdin = os.Stdin
	var out, errors strings.Builder
	command.Stdout = &out
	command.Stderr = &errors

	var result Result
	result.Code, result.Failed = Ended(command.Run())
	result.Out, result.Err = out.String(), errors.String()
	return result
}

// Ended turns what a command returned into an exit code and, when the
// command did not run at all, the reason.
//
// A driver that treated "could not start" as an exit code would count a
// missing binary as a measurement.
func Ended(err error) (int, error) {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit):
		return exit.ExitCode(), nil
	default:
		return -1, err
	}
}
