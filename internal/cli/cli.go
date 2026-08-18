package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/dlaszlo/camp/internal/config"
	"github.com/dlaszlo/camp/internal/fsx"
	"github.com/dlaszlo/camp/internal/gen"
	"github.com/dlaszlo/camp/internal/health"
	"github.com/dlaszlo/camp/internal/logs"
	"github.com/dlaszlo/camp/internal/mountinfo"
	"github.com/dlaszlo/camp/internal/plan"
	"github.com/dlaszlo/camp/internal/preflight"
	"github.com/dlaszlo/camp/internal/refusal"
	"github.com/dlaszlo/camp/internal/report"
	"github.com/dlaszlo/camp/internal/reports"
)

// Version is stamped at build time; the zero value is honest about that.
var Version = "dev"

// command is one subcommand: its name, one line of help, and how to run it.
type command struct {
	name    string
	summary string
	run     func(ctx *context, args []string) error
}

// context is what every command is given. Keeping the streams here rather
// than reaching for os.Stdout means the commands can be tested.
//
// The two streams are two different things and not two habits. stdout is
// the command's product -- what a reader pipes: the plan, the description,
// the listings. err is everything about the run, and it is a sink rather
// than a stream because every line of it is also kept in camp's own log.
type context struct {
	out io.Writer
	err *report.Sink
}

func (c *context) printf(format string, args ...any) {
	fmt.Fprintf(c.out, format, args...)
}

// keep starts writing this environment's log, and says so once if it
// cannot.
//
// Always on: a log somebody has to switch on is missing on exactly the
// run that surprised them. A run that cannot write one still has work to
// do, so this reports and carries on rather than refusing -- but it does
// report, because a record silently not being kept is worse than no
// record.
func (c *context) keep(cfg config.Config) {
	c.attach(cfg.Env)
}

// keepUnder starts the log of the environment a configuration path
// belongs to, whether or not that file still reads as a configuration:
// the path of the file is enough to know which .camp it lived in, and a
// teardown running against a broken configuration is a run whose record
// somebody will want.
func (c *context) keepUnder(source string) {
	if source == "" {
		return
	}
	// $ENV/.camp/config.yml, so the environment root is two above it.
	c.attach(filepath.Dir(filepath.Dir(source)))
}

func (c *context) attach(env string) {
	file, err := logs.Open(env)
	if err != nil {
		report.Narrate(c.err).Warn("camp's log is not being written: %v. "+
			"Nothing else about this run changes.", err)
		return
	}
	c.err.Keep(file)
}

func commands() []command {
	listed := composeCommands()
	return append(listed,
		command{"plan", "print what would be mounted, and why", cmdPlan},
		command{"doctor", "what this machine and this configuration lack", cmdDoctor},
		command{"init", "write a " + config.Dir + "/" + config.FileName + " to start from", cmdInit},
	)
}

// Main parses arguments, runs one command and returns an exit code.
func Main(args []string, out, errOut io.Writer) int {
	say := report.To(errOut)
	defer say.Close()
	ctx := &context{out: out, err: say}

	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(ctx)
		return ExitOK
	}
	if args[0] == "--version" || args[0] == "version" {
		ctx.printf("camp %s\n", Version)
		return ExitOK
	}

	for _, candidate := range commands() {
		if candidate.name != args[0] {
			continue
		}
		if err := candidate.run(ctx, args[1:]); err != nil {
			fmt.Fprintln(ctx.err, render(err))
			// The log is closed here rather than left to the deferred call,
			// because a command's last word is the one most worth keeping and
			// some exits do not run deferred calls at all.
			ctx.err.Close()
			return exitCode(err)
		}
		return ExitOK
	}

	fmt.Fprintf(ctx.err, "error: no command called %q\n", args[0])
	usage(ctx)
	return ExitUsage
}

func usage(ctx *context) {
	ctx.printf("camp composes several git repositories into one working " +
		"directory,\nwithout any of them learning about the others.\n\n")
	ctx.printf("usage: camp <command> [options]\n\n")
	width := 0
	for _, c := range commands() {
		if len(c.name) > width {
			width = len(c.name)
		}
	}
	for _, c := range commands() {
		ctx.printf("  %-*s  %s\n", width, c.name, c.summary)
	}
	ctx.printf("\nrun 'camp <command> -h' for the options of one command\n")
}

// -- shared plumbing --------------------------------------------------------

func flagsFor(name string) (*flag.FlagSet, *string) {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	file := set.String("f", "", "path to a "+config.Dir+"/"+config.FileName+
		" (default: found in this directory or above)")
	return set, file
}

// resolve finds the composition a command should act on: the one named by
// -f, or the one this directory belongs to.
//
// It is also where the log is attached, because this is the moment camp
// knows which environment it is working in: the log lives under that
// environment's own .camp, and nothing before this point could have named
// a file to write.
func resolve(ctx *context, file string) (config.Config, error) {
	path := file
	if path == "" {
		start, err := os.Getwd()
		if err != nil {
			return config.Config{}, wrap(err, ExitFailure, "")
		}
		found, err := config.Find(start)
		if err != nil {
			return config.Config{}, wrap(err, ExitNotFound, "")
		}
		path = found
	}

	cfg, err := config.Load(path)
	if err != nil {
		var refused refusal.List
		if errors.As(err, &refused) {
			return config.Config{}, failure(ExitUsage, "",
				"%s cannot be read:\n\n%s", path, report.Refusals(refused))
		}
		return config.Config{}, wrap(err, ExitUsage, "")
	}

	ctx.keep(cfg)

	// A namespace session leaves its findings in a file, because by the
	// time its last window closes there is nobody to print them to. This
	// is where they reach somebody: once, and then marked as read.
	reports.Show(cfg.Env, func(text string) {
		fmt.Fprintf(ctx.err, "%s\n", text)
	})
	return cfg, nil
}

func parseMode(privileged bool) plan.Mode {
	if privileged {
		return plan.Privileged
	}
	return plan.Namespace
}

// -- commands ---------------------------------------------------------------

func cmdPlan(ctx *context, args []string) error {
	set, file := flagsFor("plan")
	systemWide := set.Bool("privileged", false,
		"plan for the system-wide mode instead of the namespace mode")
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}
	cfg, err := resolve(ctx, *file)
	if err != nil {
		return err
	}

	built, refused := plan.Prepare(cfg, parseMode(*systemWide))
	generated, problems := gen.Preview(built)
	refused.Extend(problems)
	if len(built.Mounts) > 0 {
		expanded := gen.Expand(built, generated)
		expanded.Warnings = built.Warnings
		ctx.printf("%s", report.Plan(expanded))
		ctx.printf("%s", report.Expansion(built, generated))
		ctx.printf("the mount calls, in order:\n%s\n", report.Syscalls(expanded))
	}
	if !refused.Empty() {
		fmt.Fprintf(ctx.err, "this composition would not start. %d thing(s) "+
			"stop it:\n\n%s", refused.Count(), report.Refusals(refused))
		return failure(ExitPrecondition, "",
			"nothing was mounted, and nothing has to be undone -- every one of "+
				"these can be fixed by hand right now")
	}
	ctx.printf("%s\n", plan.GateSummary(cfg, built.LowerRoot, built.UpperRoot))
	ctx.printf("nothing stops this composition.\n")
	return nil
}

func cmdDoctor(ctx *context, args []string) error {
	set, file := flagsFor("doctor")
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}

	// Each mode is reported separately: one being unavailable is not a
	// failure as long as the other works.
	var usable []preflight.Mode
	for _, mode := range []preflight.Mode{preflight.Namespace, preflight.Privileged} {
		checks := preflight.Run(mode)
		ctx.printf("%s mode:\n%s\n", mode, report.Checks(checks))
		if len(preflight.Failures(checks)) == 0 {
			usable = append(usable, mode)
		}
	}
	switch len(usable) {
	case 0:
		ctx.printf("no mode is available on this machine.\n\n")
	case 1:
		ctx.printf("usable mode: %s\n\n", usable[0])
	default:
		ctx.printf("both modes are available.\n\n")
	}

	cfg, err := resolve(ctx, *file)
	if err != nil {
		fmt.Fprintf(ctx.err, "%s\n", render(err))
		if len(usable) == 0 {
			return failure(ExitPrecondition, "", "this machine is missing something camp needs")
		}
		return nil
	}

	ctx.printf("composition: %s\n\n", report.ConfigSummary(cfg))
	built, refused := plan.Prepare(cfg, plan.Namespace)
	if !refused.Empty() {
		fmt.Fprintf(ctx.err, "this composition would not start. %d thing(s) "+
			"stop it:\n\n%s", refused.Count(), report.Refusals(refused))
		return failure(ExitPrecondition, "", "something above has to be fixed first")
	}
	ctx.printf("%s", plan.GateSummary(cfg, built.LowerRoot, built.UpperRoot))
	ctx.printf("the configuration is sound: %d mounts, nothing refused.\n", len(built.Mounts))

	if len(built.Warnings) > 0 {
		ctx.printf("\nworth knowing (none of these stop a composition):\n")
		for _, warning := range built.Warnings {
			ctx.printf("  %s\n", warning)
		}
	}

	table, tableErr := mountinfo.Read(mountinfo.Self)
	if tableErr == nil {
		if notes := health.Look(cfg, built, table); len(notes) > 0 {
			ctx.printf("\nthis environment:\n%s", health.Render(notes))
		}
	}

	if len(usable) == 0 {
		return failure(ExitPrecondition, "", "this machine is missing something camp needs")
	}
	return nil
}

// cmdInit writes the skeleton and refuses to overwrite an existing one.
//
// No --force: a configuration that is already there was written by
// somebody for a reason, and replacing it is their move, not camp's.
func cmdInit(ctx *context, args []string) error {
	set := flag.NewFlagSet("init", flag.ContinueOnError)
	if err := set.Parse(args); err != nil {
		return wrap(err, ExitUsage, "")
	}

	directory := "."
	if set.NArg() > 0 {
		directory = set.Arg(0)
	}
	env, err := filepath.Abs(directory)
	if err != nil {
		return wrap(err, ExitFailure, "")
	}

	target := config.Path(env)
	if _, err := os.Stat(target); err == nil {
		return failure(ExitPrecondition,
			"edit it, or move it aside first -- camp does not overwrite a "+
				"configuration somebody wrote",
			"%s already exists", target)
	}
	area := fsx.Camp(filepath.Dir(target))
	if err := area.Ensure(0o755); err != nil {
		return wrap(err, ExitFailure, "")
	}
	if err := area.Write(config.FileName, []byte(report.ConfigTemplate(env)), 0o644); err != nil {
		return wrap(err, ExitFailure, "")
	}
	// The ignore rules live in camp's own directory, so they hold whichever
	// repository the environment root belongs to -- or none.
	if err := area.Write(".gitignore", []byte(report.CampIgnore), 0o644); err != nil {
		return wrap(err, ExitFailure, "")
	}
	// And a note in the directory itself saying which of the things in it
	// is the reader's and which is camp's working material.
	if err := area.Write("README.md", []byte(report.CampReadme), 0o644); err != nil {
		return wrap(err, ExitFailure, "")
	}
	ctx.printf("wrote %s\n"+
		"and, beside it, a README saying which of the things in %s are "+
		"yours and which are camp's, and a .gitignore that keeps camp's "+
		"scratch out of version control.\n"+
		"config.yml is the one file you edit. Every CHANGE-ME has to become "+
		"a real directory name; then run 'camp plan' to see what it would "+
		"do.\n", target, filepath.Dir(target))
	return nil
}
