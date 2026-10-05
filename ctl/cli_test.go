package ctl

import (
	"bytes"
	"context"
	"flag"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// testCLI is a minimal client: enough of one to exercise the dispatch, the help
// rendering and the flag reordering without any of it depending on a particular
// daemon's commands.
func testCLI(ran *[]string) *CLI {
	return &CLI{
		Name:      "testctl",
		App:       "testd",
		SocketEnv: "TESTD_CTL_SOCKET",
		UsageLine: "Usage: testctl [flags] <command> [flags] [args]",
		Intro:     "Talks to testd.",
		Footer:    "Exit status is 0, 1 or 2.",
		Commands: []Command{
			{
				Name:  "ping",
				Usage: "check that a daemon is answering",
				Run: func(_ context.Context, iv *Invoker, args []string) int {
					fs := iv.FlagSet("ping")
					if code := ParseFlags(fs, args); code != Continue {
						return code
					}
					return 0
				},
			},
			{
				Name:  "pair",
				Usage: "a command with subcommands",
				Help: `Subcommands:
  a
      the first
  b
      the second`,
				Run: func(_ context.Context, iv *Invoker, args []string) int {
					if len(args) == 0 {
						iv.Usage()
						return UsageError
					}
					*ran = append(*ran, args[0])
					return 0
				},
			},
		},
	}
}

func TestCLIUsageComesFromTheTable(t *testing.T) {
	var buf bytes.Buffer
	testCLI(nil).Usage(&buf)
	out := buf.String()
	for _, want := range []string{
		"Usage: testctl [flags] <command> [flags] [args]",
		"Talks to testd.",
		"Commands:",
		"  ping",
		"check that a daemon is answering",
		"  pair",
		"a command with subcommands",
		"Subcommands:",
		"      a",
		"Global flags:",
		"-socket",
		"-timeout",
		"-verbose",
		"Exit status is 0, 1 or 2.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help does not contain %q:\n%s", want, out)
		}
	}
}

// A command table is only worth having if the help is generated from it, so a
// command nobody bothered to document must not silently vanish from --help.
func TestCLIUsageListsEveryCommand(t *testing.T) {
	var buf bytes.Buffer
	cli := testCLI(nil)
	cli.Usage(&buf)
	for _, c := range cli.Commands {
		if !strings.Contains(buf.String(), "\n  "+c.Name+"\n") {
			t.Errorf("help omits %q", c.Name)
		}
	}
}

// "help" is answered from the table, without a daemon, and `help <command>`
// narrows it to one command. Help goes to stdout when it was asked for and to
// stderr when it is part of a complaint, so the assertions look at both.
func TestCLIHelp(t *testing.T) {
	cases := []struct {
		args     []string
		wantCode int
		want     string
	}{
		{[]string{"help"}, HelpShown, "Usage: testctl"},
		{[]string{"--help"}, HelpShown, "Usage: testctl"},
		{[]string{"-h"}, HelpShown, "Usage: testctl"},
		{[]string{"help", "pair"}, HelpShown, "Usage: testctl pair"},
		{[]string{"help", "nope"}, UsageError, `unknown command "nope"`},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			code, combined := withOutput(t, func() int { return testCLI(nil).Run(tc.args) })
			if code != tc.wantCode {
				t.Errorf("exit = %d, want %d (output: %s)", code, tc.wantCode, combined)
			}
			if !strings.Contains(combined, tc.want) {
				t.Errorf("output does not contain %q:\n%s", tc.want, combined)
			}
		})
	}
}

// A command with subcommands dispatches them itself, from the args it is handed.
func TestCLIPassesTheRestOfTheArgsToTheCommand(t *testing.T) {
	var ran []string
	if code := testCLI(&ran).Run([]string{"pair", "b", "--extra"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(ran) != 1 || ran[0] != "b" {
		t.Errorf("command saw %v, want [b]", ran)
	}
}

// Help asked for goes to stdout; a bad flag and its remedy go to stderr. The flag
// package calls Usage on both -h and a bad flag, so a CLI that also prints its
// own help on the way out prints everything twice — and a user who pastes their
// terminal gets a wall of duplicate text to read.
func TestHelpIsPrintedExactlyOnce(t *testing.T) {
	cases := []struct {
		args       []string
		wantStdout bool
	}{
		{[]string{"--help"}, true},
		{[]string{"-h"}, true},
		{[]string{"help"}, true},
		{[]string{"--nope"}, false},
		{[]string{"nonsense"}, false},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			code, out := withOutput(t, func() int { return testCLI(nil).Run(tc.args) })
			if code == HelpShown != tc.wantStdout {
				t.Errorf("exit = %d, HelpShown = %v", code, code == HelpShown)
			}
			if got := strings.Count(out, "Usage: testctl"); got != 1 {
				t.Errorf("usage line appears %d times, want once:\n%s", got, out)
			}
		})
	}
}

func TestCLIUsageErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"nonsense"},
		{"pair"},
		{"ping", "--nope"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var ran []string
			code, out := withOutput(t, func() int { return testCLI(&ran).Run(args) })
			if code != UsageError {
				t.Errorf("exit = %d, want %d (output: %s)", code, UsageError, out)
			}
			if len(ran) != 0 {
				t.Errorf("a command ran for a usage error: %v", ran)
			}
			if out == "" {
				t.Error("a usage error explained nothing")
			}
		})
	}
}

// ParseFlags exists because the stdlib stops at the first non-flag token, so
// "list opensuse/tumbleweed --verbose" would otherwise fail with a spurious
// "needs exactly one argument". Which flags take a value is answered by the
// FlagSet itself, so a positional following a value flag is not misread.
func TestParseFlagsReordersAroundPositionals(t *testing.T) {
	cases := []struct {
		in         []string
		wantPos    []string
		wantMinAge time.Duration
		wantDryRun bool
	}{
		{[]string{"--dry-run", "target"}, []string{"target"}, 0, true},
		{[]string{"target", "--dry-run"}, []string{"target"}, 0, true},
		{[]string{"target", "--min-age", "2h", "list"}, []string{"target", "list"}, 2 * time.Hour, false},
		{[]string{"--min-age=2h", "target"}, []string{"target"}, 2 * time.Hour, false},
		{[]string{"--", "--dry-run"}, []string{"--dry-run"}, 0, false},
		{[]string{"-"}, []string{"-"}, 0, false},
		// A value that looks like a flag is still a value: --min-age -1h must
		// not be read as a flag named "-1h".
		{[]string{"--min-age", "-1h", "target"}, []string{"target"}, -time.Hour, false},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.in, " "), func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			dryRun := fs.Bool("dry-run", false, "")
			minAge := fs.Duration("min-age", 0, "")
			if code := ParseFlags(fs, tc.in); code != Continue {
				t.Fatalf("ParseFlags = %d, want Continue", code)
			}
			if got := strings.Join(fs.Args(), " "); got != strings.Join(tc.wantPos, " ") {
				t.Errorf("positionals = %q, want %q", got, tc.wantPos)
			}
			if *minAge != tc.wantMinAge {
				t.Errorf("--min-age = %v, want %v", *minAge, tc.wantMinAge)
			}
			if *dryRun != tc.wantDryRun {
				t.Errorf("--dry-run = %v, want %v", *dryRun, tc.wantDryRun)
			}
		})
	}
}

func TestParseFlagsReportsHelpAndErrors(t *testing.T) {
	t.Run("help", func(t *testing.T) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		if code := ParseFlags(fs, []string{"-h"}); code != HelpShown {
			t.Errorf("ParseFlags(-h) = %d, want %d", code, HelpShown)
		}
	})
	t.Run("bad flag", func(t *testing.T) {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		if code := ParseFlags(fs, []string{"--nope"}); code != UsageError {
			t.Errorf("ParseFlags(--nope) = %d, want %d", code, UsageError)
		}
	})
}

// IsSet answers "did the user say so", which is not the same question as "does it
// hold a non-zero value": a zero duration can be a meaningful setting.
func TestIsSet(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	interval := fs.Duration("interval", 0, "")
	_ = fs.Duration("other", 0, "")
	if code := ParseFlags(fs, []string{"--interval", "0"}); code != Continue {
		t.Fatal("parse failed")
	}
	if !IsSet(fs, "interval") {
		t.Error("--interval 0 was not reported as given")
	}
	if IsSet(fs, "other") {
		t.Error("--other was reported as given")
	}
	_ = interval
}

// The connection flags must mean the same thing before a command and after it,
// which is why both parses write the same variables and the later parse wins.
func TestConnectionFlagsWorkInBothPositions(t *testing.T) {
	newIV := func() *Invoker {
		cli := testCLI(nil)
		return &Invoker{cli: cli, command: cli.Commands[0]}
	}
	t.Run("after wins", func(t *testing.T) {
		iv := newIV()
		fs := flag.NewFlagSet("globals", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		iv.registerConnFlags(fs)
		if err := fs.Parse([]string{"--socket", "/first", "--timeout", "5s"}); err != nil {
			t.Fatal(err)
		}
		sub := iv.FlagSet("ping")
		if code := ParseFlags(sub, []string{"--socket", "/second"}); code != Continue {
			t.Fatalf("ParseFlags = %d", code)
		}
		if iv.socket != "/second" {
			t.Errorf("socket = %q, want /second", iv.socket)
		}
		if iv.timeout != 5*time.Second {
			t.Errorf("timeout = %v, want 5s: a flag given only before the command must survive", iv.timeout)
		}
	})
	t.Run("seeded into the subcommand", func(t *testing.T) {
		iv := newIV()
		iv.socket = "/from-globals"
		fs := iv.FlagSet("ping")
		if code := ParseFlags(fs, nil); code != Continue {
			t.Fatalf("ParseFlags = %d", code)
		}
		if iv.socket != "/from-globals" {
			t.Errorf("socket = %q, want the leading value", iv.socket)
		}
	})
}

// Resolution order: the flag, then the environment variable, then the
// conventional default.
func TestSocketPathResolution(t *testing.T) {
	cli := testCLI(nil)
	t.Run("flag", func(t *testing.T) {
		iv := &Invoker{cli: cli, socket: "/explicit"}
		got, err := iv.socketPath()
		if err != nil || got != "/explicit" {
			t.Errorf("socketPath = %q, %v", got, err)
		}
	})
	t.Run("environment", func(t *testing.T) {
		t.Setenv("TESTD_CTL_SOCKET", "/from-env")
		iv := &Invoker{cli: cli}
		got, err := iv.socketPath()
		if err != nil || got != "/from-env" {
			t.Errorf("socketPath = %q, %v", got, err)
		}
	})
	t.Run("default", func(t *testing.T) {
		t.Setenv("TESTD_CTL_SOCKET", "")
		iv := &Invoker{cli: cli}
		got, err := iv.socketPath()
		if err != nil {
			t.Fatal(err)
		}
		want, err := DefaultSocketPath("testd")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("socketPath = %q, want %q", got, want)
		}
	})
	t.Run("nothing to go on", func(t *testing.T) {
		iv := &Invoker{cli: &CLI{Name: "x"}}
		if _, err := iv.socketPath(); err == nil {
			t.Error("socketPath succeeded with neither a socket nor an app name")
		}
	})
}

// Client is the door to the daemon, so a caller that gets nil must be able to
// tell it apart from a client that failed to dial: nothing was asked yet, so the
// caller owes the user a usage error rather than a command failure.
func TestClientReportsAnUnresolvableSocket(t *testing.T) {
	iv := &Invoker{cli: &CLI{Name: "testctl"}}
	var c *Client
	withOutput(t, func() int {
		c = iv.Client()
		return Continue
	})
	if c != nil {
		t.Errorf("Client = %v, want nil: there is no socket to dial", c)
	}
}

// withOutput runs fn with os.Stdout and os.Stderr redirected, and returns fn's
// result together with everything written to them. The diagnostics this package
// emits go to the process's own streams rather than an injected writer, because
// a CLI's terminal is the only place an operator will look.
func withOutput(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	var wg sync.WaitGroup
	streams := []**os.File{&os.Stdout, &os.Stderr}
	original := make(map[**os.File]*os.File)
	bufs := make(map[**os.File]*bytes.Buffer)
	var readers, writers []*os.File
	for _, stream := range streams {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		// One buffer per stream: two readers draining into one bytes.Buffer
		// would race, and a test helper that loses output is worse than none.
		buf := &bytes.Buffer{}
		bufs[stream] = buf
		wg.Add(1)
		go func(r *os.File, buf *bytes.Buffer) {
			defer wg.Done()
			_, _ = io.Copy(buf, r)
		}(r, buf)
		readers, writers = append(readers, r), append(writers, w)
		original[stream] = *stream
		*stream = w
	}
	code := fn()
	for stream, saved := range original {
		*stream = saved
	}
	// Close the write ends first and let the readers reach EOF; closing a read
	// end early throws away whatever is still sitting in the pipe.
	for _, w := range writers {
		w.Close()
	}
	wg.Wait()
	for _, r := range readers {
		r.Close()
	}
	return code, bufs[&os.Stdout].String() + bufs[&os.Stderr].String()
}

// Client resolves the socket once: a command that asked for it twice should not
// dial twice, and should not re-derive the path from an environment that may
// have changed since.
func TestClientCachesTheResolvedClient(t *testing.T) {
	iv := &Invoker{cli: testCLI(nil), socket: "/tmp/somewhere/socket"}
	first := iv.Client()
	if first == nil {
		t.Fatal("Client = nil")
	}
	if first.Path != "/tmp/somewhere/socket" {
		t.Errorf("Path = %q", first.Path)
	}
	if second := iv.Client(); second != first {
		t.Error("Client returned a second client; the socket should be resolved once")
	}
}
