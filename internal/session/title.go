package session

import (
	"fmt"
	"os"
	"path/filepath"
	"unicode"
)

// nameTerminal says on the terminal that a session is running, and puts
// the name back when it ends.
//
// What it answers: inside a composition nothing looks different. The
// prompt is the shell's, the tree is a directory like any other, and the
// one honest signal -- that the working directory is the composed tree --
// is the part of a prompt people stop reading. A terminal's title is the
// one place a tool may write that belongs to that terminal for exactly as
// long as the session does, that no program has to cooperate with, and
// that leaves nothing behind: the title is pushed on the terminal's own
// stack and popped when the session ends.
//
// What it does not answer, and this is worth knowing before relying on
// it: a shell that writes the title itself overwrites this at its first
// prompt. Debian's and Ubuntu's default bashrc does exactly that -- it
// puts a title escape inside PS1 for xterm-like terminals -- so under
// 'camp shell' with a stock bashrc the title is the shell's again as soon
// as the first prompt is drawn. It holds for everything else: an editor,
// an agent, a build, any 'camp run -- <program>' that does not write
// titles of its own. A session that wants the marker in the prompt as
// well declares it in session.environment, which is the composition's to
// decide and not this program's.
//
// Nothing is written to something that is not a terminal. An escape
// sequence in a log or a pipe is not a marker, it is corruption of
// somebody's output.
func nameTerminal(stream *os.File, environment string) func() {
	if stream == nil || !terminal(stream) {
		return func() {}
	}
	name := printable(filepath.Base(environment))
	if name == "" {
		return func() {}
	}

	// Push, then set. The pop is what makes this borrowing rather than
	// taking: xterm and every terminal that followed it keep a stack for
	// exactly this, so the title a person chose comes back without camp
	// having to read it -- which cannot be done portably anyway.
	fmt.Fprintf(stream, "\x1b[22;0t\x1b]0;camp: %s\x07", name)
	return func() { fmt.Fprint(stream, "\x1b[23;0t") }
}

// printable keeps what is safe to put inside an escape sequence.
//
// The name comes from a directory, and a directory name may hold anything
// but a slash and a null -- including an escape character. Writing one
// through would let whoever can name a directory write to the terminal of
// whoever composes it, which is somebody else's cursor, somebody else's
// title, and on some terminals somebody else's command line. So: no
// control characters, and a length a title bar can hold.
func printable(name string) string {
	const most = 64
	kept := make([]rune, 0, most)
	for _, r := range name {
		if unicode.IsControl(r) {
			continue
		}
		if len(kept) == most {
			break
		}
		kept = append(kept, r)
	}
	return string(kept)
}
