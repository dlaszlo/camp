// Package envx is the environment a session's workload receives: the
// grammar of what a configuration may declare, and the arithmetic that
// turns declarations into one list for execve.
//
// Everything here is pure. It reads no file, starts no process, and never
// touches the environment of the process it runs in -- it is handed a base
// map and returns strings. That is deliberate: the resolution happens in
// camp's own init while that process still holds CAP_SYS_ADMIN, and the
// only safe thing to do with a declared value at that moment is to build
// an inert string out of it. Nothing here can install one anywhere.
//
// There is no shell in any of this. `$NAME` and `${NAME}` insert the bytes
// a name had in the environment camp was started with, `$$` is one literal
// dollar, and that is the whole language: no command substitution, no
// defaulting, no `~`, no word splitting, no globbing, no quote or
// backslash processing. Inserted bytes are never scanned again, so a value
// arriving through `$HOME` that itself contains `$X` contributes those
// three literal bytes.
//
// Two rules in here exist because their absence is a specific,
// already-measured kind of lie:
//
// An absent name refuses instead of expanding to empty. Silently replacing
// a typo with nothing produces a value that looks applied and is not,
// which is the failure this whole design exists to prevent.
//
// A bare command is looked up against the *effective* PATH -- the one the
// workload will actually have -- and never against camp's own. Resolving
// against camp's path while printing the declared one would select the
// host's command under a plan that says otherwise.
package envx

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/dlaszlo/camp/internal/enc"
	"github.com/dlaszlo/camp/internal/refusal"
)

const (
	// Prefix marks camp's own names. A configuration cannot declare one,
	// and only Live is an interpolation input.
	Prefix = "CAMP_"
	// Live is the composed tree's path, camp's authoritative value for it.
	Live = "CAMP_LIVE"
	// Cwd is the workload's working directory. camp sets the directory and
	// this value together, so a declaration disagreeing with reality would
	// put a lie in the environment.
	Cwd = "PWD"
)

// Expr is one declared value: literal bytes with references into the base
// environment, in the order they were written.
type Expr struct {
	name  string
	parts []part
}

// part is either literal bytes or one reference. A reference carries no
// text and literal bytes carry no name; the two never mix in one part.
type part struct {
	text string
	name string
}

// Name returns the variable this expression was declared for.
func (e Expr) Name() string { return e.name }

// References returns every name the expression reads, in order of
// appearance, so a caller can say what a resolution will need before it
// tries.
func (e Expr) References() []string {
	names := make([]string, 0, len(e.parts))
	for _, p := range e.parts {
		if p.name != "" {
			names = append(names, p.name)
		}
	}
	return names
}

// CheckName refuses a name that could not become one execve entry.
//
// A child receives its environment as a list of `name=value` strings, so a
// name that is empty or that contains `=` or NUL does not name one
// variable at all, whatever the file says it does.
func CheckName(name string) error {
	switch {
	case name == "":
		return refusal.New("environment-name",
			"a session.environment entry has an empty name.\n"+
				"A workload receives each variable as one 'name=value' entry, and an "+
				"empty name cannot form one. Give it a name, or remove the entry.")
	case strings.Contains(name, "="):
		return refusal.New("environment-name",
			"the environment name %q contains '='.\n"+
				"A workload receives each variable as one 'name=value' entry, so the "+
				"first '=' is where camp would have to say the name ends -- and this "+
				"name would then be a different one from the one written here. Rename "+
				"the key.", enc.Encode(name))
	case strings.ContainsRune(name, 0):
		return refusal.New("environment-name",
			"the environment name %q contains a NUL byte.\n"+
				"An environment entry is a C string: it ends at the first NUL, so the "+
				"name a workload would receive is not the one written here. Rename the "+
				"key.", enc.Encode(name))
	case name == Cwd:
		return refusal.New("environment-reserved",
			"session.environment cannot declare %s.\n"+
				"camp sets the workload's working directory and this value together, "+
				"so a declared one would disagree with where the workload actually "+
				"stands -- a lie in the environment rather than a setting. The "+
				"composed tree's path is available as $%s if a value needs it.",
			Cwd, Live)
	case strings.HasPrefix(name, Prefix):
		return refusal.New("environment-reserved",
			"session.environment cannot declare %s: names beginning %s are camp's "+
				"own.\n"+
				"camp sets %s to the composed tree's path, and reserves the rest of "+
				"the prefix so that a name camp adds later cannot collide with a "+
				"configuration written today. Reference $%s in another value, or "+
				"choose a name of your own.",
			enc.Encode(name), Prefix, Live, Live)
	}
	return nil
}

// Parse reads one declared value into an expression.
//
// Every problem it can see without an environment is found here, so that
// the configuration reader reports all of them in one pass: NUL bytes, and
// every malformed `$` form. What is left for resolution is exactly the
// question an environment answers -- whether a referenced name is set.
func Parse(name, value string) (Expr, error) {
	if index := strings.IndexByte(value, 0); index >= 0 {
		return Expr{}, refusal.New("environment-value",
			"session.environment.%s contains a NUL byte at byte %d.\n"+
				"An environment entry is a C string and ends at the first NUL, so "+
				"everything after it would be dropped on the way to the workload. "+
				"Remove it, or encode it the way the program reading this value "+
				"expects.", name, index)
	}

	expression := Expr{name: name}
	var literal strings.Builder
	flush := func() {
		if literal.Len() > 0 {
			expression.parts = append(expression.parts, part{text: literal.String()})
			literal.Reset()
		}
	}

	for index := 0; index < len(value); {
		if value[index] != '$' {
			literal.WriteByte(value[index])
			index++
			continue
		}
		if index+1 < len(value) && value[index+1] == '$' {
			literal.WriteByte('$')
			index += 2
			continue
		}
		reference, width, err := readReference(name, value, index)
		if err != nil {
			return Expr{}, err
		}
		flush()
		expression.parts = append(expression.parts, part{name: reference})
		index += width
	}
	flush()

	for _, reference := range expression.References() {
		if reference == Cwd {
			return Expr{}, refusal.New("environment-pwd",
				"session.environment.%s refers to $%s.\n"+
					"The directory the camp command was run from is not carried into "+
					"the session, so this reference has no unambiguous meaning: it is "+
					"neither the invoking directory nor the workload's. The composed "+
					"tree is what the workload stands in -- write $%s.",
				name, Cwd, Live)
		}
	}
	return expression, nil
}

// readReference reads one `$NAME` or `${NAME}` starting at the dollar.
//
// Both forms take the same name grammar. The braced form exists to
// separate a name from the text after it -- `${CAMP_LIVE}bin` -- and not
// to admit names the bare form would not take: one grammar is a rule a
// reader can hold, two is a corner nobody tests.
func readReference(declaration, value string, at int) (string, int, error) {
	malformed := func(explanation string) error {
		return refusal.New("environment-expansion",
			"session.environment.%s has a '$' at byte %d that camp cannot read: "+
				"%s.\n"+
				"Write $NAME or ${NAME} to insert the value a name had in the "+
				"environment camp was started with, and $$ for a literal dollar. "+
				"There is no shell here: nothing else after a dollar means anything. "+
				"The value reads: %s",
			declaration, at, explanation, enc.Encode(value))
	}

	if at+1 < len(value) && value[at+1] == '{' {
		end := strings.IndexByte(value[at+2:], '}')
		if end < 0 {
			return "", 0, malformed("the '${' is never closed by a '}'")
		}
		name := value[at+2 : at+2+end]
		if !identifier(name) {
			return "", 0, malformed(fmt.Sprintf("%q is not a name -- a name starts "+
				"with a letter or '_' and continues with letters, digits or '_'",
				enc.Encode(name)))
		}
		return name, end + 3, nil
	}

	end := at + 1
	for end < len(value) && continues(value[end], end == at+1) {
		end++
	}
	if end == at+1 {
		return "", 0, malformed("nothing that could be a name follows it")
	}
	return value[at+1 : end], end - at, nil
}

func identifier(name string) bool {
	if name == "" {
		return false
	}
	for index := 0; index < len(name); index++ {
		if !continues(name[index], index == 0) {
			return false
		}
	}
	return true
}

func continues(character byte, first bool) bool {
	switch {
	case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z',
		character == '_':
		return true
	case character >= '0' && character <= '9':
		return !first
	default:
		return false
	}
}

// Base is what references read: the environment the camp command was
// started with, plus camp's one authoritative value.
//
// Declarations never read each other. They all resolve against this one
// base, which is what makes the mapping's order meaningless and a cycle
// impossible to write.
type Base struct {
	values map[string]string
	live   string
}

// NewBase takes the base from an environ list and the live path.
//
// Inherited CAMP_* names are dropped on the way in. A session started
// inside another session would otherwise read the outer composition's live
// path while entering the inner one, and every other camp name is camp's
// to define rather than a value to build on.
func NewBase(environ []string, live string) Base {
	values := make(map[string]string, len(environ))
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.HasPrefix(name, Prefix) {
			continue
		}
		// The first wins, the way getenv answers when a list carries a name
		// twice.
		if _, seen := values[name]; !seen {
			values[name] = value
		}
	}
	return Base{values: values, live: live}
}

// LivePath is camp's authoritative value for the composed tree.
func (b Base) LivePath() string { return b.live }

// Resolve produces the bytes the workload will receive.
func (e Expr) Resolve(base Base) (string, error) {
	var out strings.Builder
	for _, p := range e.parts {
		switch {
		case p.name == "":
			out.WriteString(p.text)
		case p.name == Live:
			out.WriteString(base.live)
		case strings.HasPrefix(p.name, Prefix):
			return "", refusal.New("environment-undefined",
				"session.environment.%s refers to $%s, which is not something camp "+
					"reads back.\n"+
					"Names beginning %s are camp's own, and only $%s is an "+
					"interpolation input -- it is the composed tree this session is "+
					"entering, which is not necessarily the one the command was run "+
					"from. Use $%s, or a name of your own.",
				e.name, p.name, Prefix, Live, Live)
		default:
			value, ok := base.values[p.name]
			if !ok {
				return "", refusal.New("environment-undefined",
					"session.environment.%s refers to $%s, and %s is not set in the "+
						"environment that started camp.\n"+
						"An absent name is not the same as a name set to the empty "+
						"string, and quietly substituting nothing would make a "+
						"misspelling look applied. Correct the name, set %s before "+
						"running camp, or write $$ if the dollar was meant literally.",
					e.name, p.name, p.name, p.name)
			}
			out.WriteString(value)
		}
	}
	return out.String(), nil
}

// Display reconstructs an expression for a report, safely.
//
// Literal text is shown as it stands -- it is already in the file being
// described -- and camp's own live path is expanded, because the plan
// prints that path several lines further up anyway. Every other insertion
// is rendered as `<inherited NAME>` and never as its bytes: plan and
// explain output lands in terminals, in issues and in agent transcripts,
// and an inherited token must not be copied into one because somebody
// asked what would mount. There is no redaction by name here -- guessing
// which values are secret would hide some and miss others without a rule.
func (e Expr) Display(live string) string {
	var pieces []string
	var literal strings.Builder
	flush := func() {
		if literal.Len() > 0 {
			pieces = append(pieces, `"`+enc.Encode(literal.String())+`"`)
			literal.Reset()
		}
	}
	for _, p := range e.parts {
		switch {
		case p.name == "":
			literal.WriteString(p.text)
		case p.name == Live:
			literal.WriteString(live)
		default:
			flush()
			pieces = append(pieces, "<inherited "+p.name+">")
		}
	}
	flush()
	if len(pieces) == 0 {
		return `""`
	}
	return strings.Join(pieces, " + ")
}

// Setting is one resolved name and its bytes, on the way to execve.
type Setting struct {
	Name  string
	Value string
}

// Entry renders a setting the way execve takes it.
func (s Setting) Entry() string { return s.Name + "=" + s.Value }

// Effective builds the workload's environment: one list, no name twice.
//
// Inherited entries keep their relative order and their bytes, so nothing
// the caller had is reordered for camp's convenience. An entry a
// declaration overrides is replaced where it stands; names the inherited
// environment did not have are appended in byte order, so that two
// orderings of the same configuration produce the same list. camp's own
// names come last and win outright -- they describe where the workload
// actually is, and there is nothing for them to lose an argument with.
func Effective(inherited []string, declared []Setting, owned []Setting) []string {
	overrides := make(map[string]string, len(declared))
	for _, setting := range declared {
		overrides[setting.Name] = setting.Value
	}
	campOwned := make(map[string]bool, len(owned))
	for _, setting := range owned {
		campOwned[setting.Name] = true
	}

	out := make([]string, 0, len(inherited)+len(declared)+len(owned))
	seen := make(map[string]bool, len(inherited))
	applied := make(map[string]bool, len(declared))
	for _, entry := range inherited {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if campOwned[name] {
			continue
		}
		if value, replaced := overrides[name]; replaced {
			out = append(out, Setting{Name: name, Value: value}.Entry())
			applied[name] = true
			continue
		}
		out = append(out, entry)
	}

	fresh := make([]Setting, 0, len(declared))
	for _, setting := range declared {
		if !applied[setting.Name] {
			fresh = append(fresh, setting)
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Name < fresh[j].Name })
	for _, setting := range fresh {
		out = append(out, setting.Entry())
	}

	for _, setting := range owned {
		out = append(out, setting.Entry())
	}
	return out
}

// Value reads one name out of an environ list, the way the workload will
// read it: the first entry with that name wins.
func Value(environ []string, name string) string {
	for _, entry := range environ {
		if candidate, value, ok := strings.Cut(entry, "="); ok && candidate == name {
			return value
		}
	}
	return ""
}

// Command resolves a workload's argv[0] against an explicit PATH value.
//
// Never exec.LookPath: that reads the calling process's own PATH, and the
// calling process here is camp's init, which deliberately does not have
// the session's environment. Looking there while the plan prints the
// declared PATH would run the host's command under a plan that says
// otherwise -- and the composition-owned launcher directory, which is the
// whole point of being able to declare PATH, would never be reached.
//
// An argv[0] containing a slash names a file directly and is not searched
// for, exactly as a shell and execvp treat it.
func Command(argv0, path string) (string, error) {
	if argv0 == "" {
		return "", refusal.New("workload-empty",
			"there is no command to run: the first word of the command line is empty.")
	}
	if strings.Contains(argv0, "/") {
		return argv0, nil
	}
	for _, directory := range filepath.SplitList(path) {
		if directory == "" {
			// An empty element means the current directory, which is the
			// composed tree: that is what execvp does with one, and camp does
			// not quietly mean something else by it.
			directory = "."
		}
		candidate := filepath.Join(directory, argv0)
		if executable(candidate) {
			return candidate, nil
		}
	}
	return "", refusal.New("workload-not-found",
		"cannot run %q: no executable by that name is on the PATH this session "+
			"applies.\n"+
			"That is the session's own PATH -- what the configuration declares, "+
			"over what camp was started with -- and not camp's. 'camp plan' prints "+
			"the declaration; the directories themselves are not printed here, "+
			"because a plan or an error message is not where an inherited value "+
			"belongs. Give the command by path if it is not meant to be found "+
			"through PATH.", enc.Encode(argv0))
}

// executable reports whether a path is a file this process may execute.
func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return unix.Access(path, unix.X_OK) == nil
}
