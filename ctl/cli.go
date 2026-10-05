package ctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jirib/golang-jirib-utils/logging"
)

const (
	// Continue indicates flag parsing succeeded and execution should proceed.
	Continue = -1
	// HelpShown indicates help/usage was displayed (exit code 0).
	HelpShown = 0
	// UsageError indicates invalid flags or command syntax (exit code 2).
	UsageError = 2
)

// Command describes one top-level subcommand in a control client CLI.
type Command struct {
	// Name is the command's first token (e.g. "ping", "status").
	Name string
	// Usage is the one-line summary displayed in --help.
	Usage string
	// Help provides detailed documentation or subcommand listings.
	Help string
	// Run executes the command and returns the process exit code.
	Run func(ctx context.Context, iv *Invoker, args []string) int
}

// CLI dispatches administrative commands and manages global connection flags.
type CLI struct {
	// Name identifies the client executable in diagnostics and help output.
	Name string
	// App identifies the target daemon for resolving default socket paths.
	App string
	// SocketEnv names an environment variable providing the default socket path.
	SocketEnv string
	// UsageLine overrides the usage header line.
	UsageLine string
	// Intro is descriptive prose printed before the command list in help output.
	Intro string
	// Footer is printed after flags in help output.
	Footer string
	// Commands defines the supported command set.
	Commands []Command
}

// Run executes the CLI with the provided arguments and returns the exit code.
func (cli *CLI) Run(args []string) int {
	iv := &Invoker{cli: cli}

	fs := flag.NewFlagSet(cli.Name+" [global flags]", flag.ContinueOnError)
	fs.Usage = func() {}
	iv.registerConnFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			cli.Usage(os.Stdout)
			return HelpShown
		}
		cli.Usage(os.Stderr)
		return UsageError
	}

	rest := fs.Args()
	if len(rest) == 0 {
		cli.Usage(os.Stderr)
		return UsageError
	}

	if rest[0] == helpCommand {
		return cli.help(rest[1:])
	}

	cmd, ok := cli.lookup(rest[0])
	if !ok {
		iv.Usagef("unknown command %q", rest[0])
		return UsageError
	}
	iv.command = cmd
	return cmd.Run(context.Background(), iv, rest[1:])
}

const helpCommand = "help"

func (cli *CLI) help(args []string) int {
	iv := &Invoker{cli: cli}
	if len(args) == 0 {
		cli.Usage(os.Stdout)
		return HelpShown
	}
	cmd, ok := cli.lookup(args[0])
	if !ok {
		iv.Usagef("unknown command %q", args[0])
		return UsageError
	}
	iv.command = cmd
	iv.commandUsage(os.Stdout, iv.FlagSet(cmd.Name))
	return HelpShown
}

func (cli *CLI) lookup(name string) (Command, bool) {
	for _, c := range cli.Commands {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

// Usage writes command and flag help text to w.
func (cli *CLI) Usage(w io.Writer) {
	line := cli.UsageLine
	if line == "" {
		line = fmt.Sprintf("Usage: %s [flags] <command> [flags] [args]", cli.Name)
	}
	fmt.Fprintln(w, line)
	fmt.Fprintln(w)
	if cli.Intro != "" {
		fmt.Fprintln(w, strings.TrimRight(cli.Intro, "\n"))
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "Commands:")
	for _, c := range cli.Commands {
		fmt.Fprintf(w, "  %s\n", c.Name)
		writeIndented(w, "      ", c.text())
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Global flags:")
	fs := flag.NewFlagSet(cli.Name, flag.ContinueOnError)
	fs.SetOutput(w)
	(&Invoker{cli: cli}).registerConnFlags(fs)
	fs.PrintDefaults()
	if cli.Footer != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, strings.TrimRight(cli.Footer, "\n"))
	}
}

func (c Command) text() []string {
	var out []string
	for _, block := range []string{c.Usage, c.Help} {
		if block == "" {
			continue
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, strings.Split(strings.TrimRight(block, "\n"), "\n")...)
	}
	return out
}

func writeIndented(w io.Writer, prefix string, lines []string) {
	for _, line := range lines {
		if line == "" {
			fmt.Fprintln(w)
			continue
		}
		fmt.Fprintln(w, prefix+line)
	}
}

// Invoker carries connection configuration and client resolution for a running command.
type Invoker struct {
	cli     *CLI
	command Command

	socket  string
	timeout time.Duration
	verbose bool

	client *Client
}

// FlagSet returns a FlagSet initialized with connection flags.
func (iv *Invoker) FlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	iv.registerConnFlags(fs)
	return fs
}

func (iv *Invoker) registerConnFlags(fs *flag.FlagSet) {
	defaults := "the default socket"
	if iv.cli.App != "" {
		defaults = filepath.Join("$XDG_RUNTIME_DIR", iv.cli.App, SocketName)
	}
	if iv.cli.SocketEnv != "" {
		defaults = "$" + iv.cli.SocketEnv + ", or " + defaults
	}
	fs.StringVar(&iv.socket, "socket", iv.socket, "control socket path (default: "+defaults+")")
	fs.BoolVar(&iv.verbose, "verbose", iv.verbose, "print the socket path and each exchange to stderr")
	fs.DurationVar(&iv.timeout, "timeout", iv.timeout, "abort if the daemon does not reply within this (0 uses the default)")
}

func (iv *Invoker) commandUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintf(w, "Usage: %s %s [flags] [args]\n", iv.cli.Name, fs.Name())
	if text := iv.command.text(); len(text) > 0 {
		fmt.Fprintln(w)
		writeIndented(w, "  ", text)
	}
	fmt.Fprintln(w, "\nFlags:")
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// Client resolves and caches a Client for the configured socket path.
func (iv *Invoker) Client() *Client {
	if iv.client != nil {
		return iv.client
	}
	path, err := iv.socketPath()
	if err != nil {
		iv.Errorf("%v", err)
		return nil
	}
	c := &Client{Path: path, Timeout: iv.timeout}
	if iv.verbose {
		c.Log = logging.NewLogger(slog.LevelDebug)
	}
	iv.client = c
	return c
}

func (iv *Invoker) socketPath() (string, error) {
	if iv.socket != "" {
		return iv.socket, nil
	}
	if env := iv.cli.SocketEnv; env != "" {
		if v := os.Getenv(env); v != "" {
			return v, nil
		}
	}
	if iv.cli.App == "" {
		return "", errors.New("no socket given and no application name to derive a default from")
	}
	return DefaultSocketPath(iv.cli.App)
}

// Errorf formats and prints an error message to stderr prefixed by CLI.Name.
func (iv *Invoker) Errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, iv.cli.Name+": "+format+"\n", args...)
}

// Usage prints CLI usage to stderr.
func (iv *Invoker) Usage() { iv.cli.Usage(os.Stderr) }

// Usagef prints an error diagnostic followed by CLI usage.
func (iv *Invoker) Usagef(format string, args ...any) {
	iv.Errorf(format, args...)
	fmt.Fprintln(os.Stderr)
	iv.Usage()
}

// ParseFlags parses command arguments into fs, handling help and error output.
func (iv *Invoker) ParseFlags(fs *flag.FlagSet, args []string) int {
	fs.Usage = func() {}
	code := ParseFlags(fs, args)
	switch code {
	case HelpShown:
		iv.commandUsage(os.Stdout, fs)
	case UsageError:
		iv.commandUsage(os.Stderr, fs)
	}
	return code
}

// ParseFlags reorders args to allow flags after positional arguments, then parses them into fs.
func ParseFlags(fs *flag.FlagSet, args []string) int {
	flags, positional := splitFlagsAndArgs(fs, args)
	if err := fs.Parse(append(flags, append([]string{"--"}, positional...)...)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return HelpShown
		}
		return UsageError
	}
	return Continue
}

// IsSet reports whether a flag was explicitly passed on the command line.
func IsSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// splitFlagsAndArgs separates flags from positional arguments, checking whether
// flags consume a separate value token.
func splitFlagsAndArgs(fs *flag.FlagSet, args []string) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name, _ := strings.CutPrefix(arg, "-")
		name, _ = strings.CutPrefix(name, "-")
		name, _, hasInlineValue := strings.Cut(name, "=")
		if f := fs.Lookup(name); !hasInlineValue && f != nil && flagTakesValue(f) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, positional
}

func flagTakesValue(f *flag.Flag) bool {
	if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok {
		return !bf.IsBoolFlag()
	}
	return true
}
