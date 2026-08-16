// Package cli parses arguments, dispatches to one operation and turns a
// failure into an exit code.
//
// It holds no logic of its own. Every command resolves a composition,
// calls one thing on it and hands the result to the report package -- a
// command that starts making decisions of its own is a command whose
// behaviour cannot be tested without a terminal.
package cli

import (
	"errors"
	"fmt"

	"github.com/dlaszlo/camp/internal/report"
)

// Exit codes, stable enough for a CI job to branch on.
const (
	ExitOK           = 0
	ExitFailure      = 1 // something went wrong with no more specific code
	ExitUsage        = 2 // the command or the configuration is wrong
	ExitPrecondition = 3 // the world is not in a state where this can be done
	ExitPrivilege    = 4 // root was required and not available
	ExitBusy         = 5 // something is holding the composition
	ExitNotFound     = 6 // no such composition
)

// Error is a failure reported on purpose, carrying what to do about it
// and the exit code the shell should see.
//
// Hint is the next action, not an apology. Where there is nothing useful
// to say it is left empty rather than filled with encouragement.
type Error struct {
	Code    int
	Message string
	Hint    string
	Cause   error
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Cause }

// Render formats the error the way a person should read it.
//
// In the same marked column as everything a command says on its way here.
// A failure that renders in a shape of its own is a failure somebody has
// to find; this one is the line whose marker is not [OK].
func (e *Error) Render() string {
	out := report.Marked(report.MarkError, e.Message)
	if e.Hint != "" {
		out += "\n" + report.Marked(report.MarkHint, e.Hint)
	}
	return out
}

func failure(code int, hint string, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Hint: hint}
}

// wrap turns any error into one carrying an exit code, leaving an
// already-classified error alone.
func wrap(err error, code int, hint string) error {
	if err == nil {
		return nil
	}
	var classified *Error
	if errors.As(err, &classified) {
		return classified
	}
	return &Error{Code: code, Message: err.Error(), Hint: hint, Cause: err}
}

// exitCode reports the code an error should exit with.
func exitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Code
	}
	return ExitFailure
}

// render formats an error for the terminal.
func render(err error) string {
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Render()
	}
	return report.Marked(report.MarkError, err.Error())
}
