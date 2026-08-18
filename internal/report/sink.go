package report

import (
	"bytes"
	"fmt"
	"io"
)

// Sink is where a command says what it is doing: the stream somebody is
// watching, and the log that keeps it.
//
// It exists because a line has two destinations with different rules. The
// terminal gets it as a person reads it. The log gets the same words with
// a timestamp in front of them, which is a thing about the line's sink and
// not about the line, and so belongs here rather than in the sentence a
// check composed.
//
// It is line-oriented on purpose. A timestamp belongs to a line, so the
// log has to be handed whole lines; a partial write waits here until its
// newline arrives, and Close says the last one out.
type Sink struct {
	terminal io.Writer
	// log is nil until a command has found a configuration -- the log lives
	// under the environment's own .camp, and before that there is nowhere
	// for it to be.
	log     io.Writer
	pending []byte
	// complained is set after the log has failed once. A command that
	// cannot write its record still has work to do, and a broken log that
	// says so on every line would bury the work.
	complained bool
}

// To returns the sink a command writes its narration to.
func To(terminal io.Writer) *Sink {
	return &Sink{terminal: terminal}
}

// Keep attaches the log. Everything said after this reaches it; what a
// command said before finding its configuration reaches the terminal
// only, because until then camp does not know which environment's log it
// would be.
func (s *Sink) Keep(log io.Writer) {
	if s != nil {
		s.log = log
	}
}

// Write splits what it is given into lines and gives each to both ends.
func (s *Sink) Write(p []byte) (int, error) {
	s.pending = append(s.pending, p...)
	for {
		index := bytes.IndexByte(s.pending, '\n')
		if index < 0 {
			break
		}
		line := string(s.pending[:index])
		s.pending = append([]byte(nil), s.pending[index+1:]...)
		s.line(line)
	}
	return len(p), nil
}

// Close says the last line, if something was written without one, and
// closes the log if it owns one.
func (s *Sink) Close() error {
	if len(s.pending) > 0 {
		line := string(s.pending)
		s.pending = nil
		s.line(line)
	}
	if closer, ok := s.log.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// line is one whole line, to the terminal as it is read and to the log as
// it is kept.
func (s *Sink) line(text string) {
	fmt.Fprintf(s.terminal, "%s\n", text)
	if s.log == nil || s.complained {
		return
	}
	if _, err := s.log.Write([]byte(text + "\n")); err != nil {
		s.complained = true
		fmt.Fprintf(s.terminal, "%s\n", Marked(MarkWarn,
			fmt.Sprintf("this run is not being written to camp's log: %v. "+
				"Nothing else about the run changes.", err)))
	}
}
