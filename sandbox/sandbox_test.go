package sandbox

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jirib/golang-jirib-utils/logctx"
)

// requireBwrap skips the test if bubblewrap isn't installed, rather than
// failing — CI/dev images that haven't installed it yet shouldn't block
// unrelated work, but this package's whole point is the sandbox, so every
// test here is real (not mocked): they all exec real bwrap.
func requireBwrap(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap (bubblewrap) not installed, skipping sandbox test")
	}
}

// requireBash skips if bash isn't available; used only by the tests that
// need bash's /dev/tcp builtin to attempt an outbound connection.
func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed, skipping test that needs /dev/tcp")
	}
}

// testSpec mirrors a caller's isolation properties — read-only root,
// writable topdir, no network, fresh PID/IPC namespaces — rooted at the
// running system's / with a caller-supplied topdir, so these boundary tests
// run anywhere rather than needing a fully provisioned buildroot. The
// command and timeout are set per test.
func testSpec(topdir string) SandboxSpec {
	return SandboxSpec{
		RootFS: "/",
		Mounts: []Mount{
			WritableMount(topdir, topdir),
		},
		PrivateTmp: true,
		Network:    NetworkNone,
		Env:        []string{"HOME=" + topdir},
		Cwd:        topdir,
	}
}

// run runs command inside testSpec(topdir)'s sandbox via the bwrap backend.
func run(t *testing.T, ctx context.Context, topdir string, timeout time.Duration, command []string) (SandboxResult, error) {
	t.Helper()
	spec := testSpec(topdir)
	spec.Command = command
	spec.Timeout = timeout
	return NewBwrap().Run(ctx, spec)
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestSandbox_CanWriteInsideTopdir is the positive control: the sandbox must
// not be so restrictive that it also breaks the thing it's meant to allow.
func TestSandbox_CanWriteInsideTopdir(t *testing.T) {
	requireBwrap(t)

	topdir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(topdir, "inside-marker")

	res, err := run(t, context.Background(), topdir, 10*time.Second,
		[]string{"sh", "-c", "echo hello > " + shQuote(marker)})
	if err != nil {
		t.Fatalf("expected write inside topdir to succeed, got err=%v stderr=%s", err, res.Stderr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker file was not created inside topdir: %v", err)
	}
}

// TestSandbox_WritesNeverReachTheHost is the escape test: writes anywhere
// inside the sandbox succeed, but none of them reach persistent state. What
// matters is that the host filesystem is untouched, which is what this
// checks.
func TestSandbox_WritesNeverReachTheHost(t *testing.T) {
	requireBwrap(t)

	const outsideTarget = "/etc/golang-jirib-utils-sandbox-escape-test"
	defer os.Remove(outsideTarget) // best-effort cleanup if isolation ever fails

	_, err := run(t, context.Background(), t.TempDir(), 10*time.Second,
		[]string{"sh", "-c", "echo escaped > " + outsideTarget})
	if err == nil {
		t.Fatalf("expected the read-only test sandbox to reject this write")
	}
	if _, statErr := os.Stat(outsideTarget); statErr == nil {
		t.Fatalf("SANDBOX ESCAPE: %s was created on the host filesystem", outsideTarget)
	}
}

// TestSandbox_NoNetworkEgress proves the NetworkNone policy actually removes
// network access, so a sandboxed command can't exfiltrate data or pull
// additional payloads.
func TestSandbox_NoNetworkEgress(t *testing.T) {
	requireBwrap(t)
	requireBash(t)

	topdir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// bash's /dev/tcp pseudo-device attempts a raw TCP connect with no
	// external dependency (no curl/wget required in the sandbox). Under a
	// bare loopback-only netns this must fail immediately (ENETUNREACH),
	// not hang — the short timeout is a backstop, not the expected path.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = run(t, ctx, topdir, 5*time.Second,
		[]string{"bash", "-c", "echo -n < /dev/tcp/1.1.1.1/80"})
	if err == nil {
		t.Fatalf("expected outbound connection to fail under NetworkNone, but it succeeded")
	}
}

// TestSandbox_FreshPIDNamespace proves the sandbox isolates the process
// tree: the sandboxed process should see only itself (and maybe bwrap's own
// init), never the host's full process list.
func TestSandbox_FreshPIDNamespace(t *testing.T) {
	requireBwrap(t)

	topdir, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	res, err := run(t, context.Background(), topdir, 10*time.Second,
		[]string{"sh", "-c", "ls /proc | grep -E '^[0-9]+$' | wc -l"})
	if err != nil {
		t.Fatalf("listing /proc inside sandbox failed: %v (stderr=%s)", err, res.Stderr)
	}

	n, convErr := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if convErr != nil {
		t.Fatalf("unexpected /proc listing output %q: %v", res.Stdout, convErr)
	}
	// A handful of entries is normal (the shell + grep + wc all showing up
	// briefly); the host's actual process count would be much larger.
	if n > 10 {
		t.Fatalf("expected a near-empty fresh PID namespace, saw %d processes in /proc — PID namespace may not be isolated", n)
	}
}

// TestSandbox_MissingBwrapIsReportedNotSilentlyUnsandboxed guards against a
// deployment mistake: if bwrap is missing from the image entirely, the
// caller must fail loudly rather than silently falling back to running
// unsandboxed.
func TestSandbox_MissingBwrapIsReportedNotSilentlyUnsandboxed(t *testing.T) {
	// Deliberately run with an empty PATH so bwrap can't be found, regardless
	// of whether it's installed on the machine running this test.
	t.Setenv("PATH", t.TempDir())

	spec := testSpec(t.TempDir())
	spec.Command = []string{"true"}
	spec.Timeout = time.Second

	_, err := NewBwrap().Run(context.Background(), spec)
	if err == nil {
		t.Fatal("expected an error when bwrap is not on PATH, got nil")
	}
	if !strings.Contains(err.Error(), "bwrap") {
		t.Fatalf("expected error to mention bwrap, got: %v", err)
	}
}

func TestSandbox_EmptyCommandIsRejected(t *testing.T) {
	requireBwrap(t)

	spec := testSpec(t.TempDir())
	spec.Command = nil
	spec.Timeout = time.Second

	_, err := NewBwrap().Run(context.Background(), spec)
	if err == nil {
		t.Fatal("expected error for empty command, got nil")
	}
	if !strings.Contains(err.Error(), "command must not be empty") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// TestBwrapArgs pins the spec→bwrap translation: the semantic properties a
// caller declares must become exactly the bwrap flags the backend promises,
// and the backend's baseline confinement (dev/proc, pid/ipc namespaces,
// die-with-parent/new-session) must always be present without the caller
// asking for it.
func TestBwrapArgs(t *testing.T) {
	spec := SandboxSpec{
		Command:    []string{"sh", "-c", "true"},
		RootFS:     "/rootfs",
		Mounts:     []Mount{ReadOnlyMount("/src", "/tmp/src"), WritableMount("/out", "/tmp/out")},
		PrivateTmp: true,
		Network:    NetworkNone,
		Env:        []string{"HOME=/root"},
		Cwd:        "/tmp/out",
	}
	args := bwrapArgs(spec)

	requirePair := func(flag, value string) {
		t.Helper()
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag && args[i+1] == value {
				return
			}
		}
		t.Fatalf("expected %s %s in bwrap args, got %v", flag, value, args)
	}
	requireFlag := func(flag string) {
		t.Helper()
		if !slices.Contains(args, flag) {
			t.Errorf("expected %s in bwrap args, got %v", flag, args)
		}
	}

	// The root filesystem is bind-mounted read-only at "/", always first.
	if len(args) < 3 || args[0] != "--ro-bind" || args[1] != "/rootfs" || args[2] != "/" {
		t.Fatalf("expected root read-only bind at \"/\" first, got %v", args)
	}
	requirePair("--dev", "/dev")
	requirePair("--proc", "/proc")
	requirePair("--tmpfs", "/tmp")
	requirePair("--ro-bind", "/src")
	requirePair("--bind", "/out")
	requirePair("--setenv", "HOME")
	requirePair("--chdir", "/tmp/out")
	requireFlag("--unshare-net")
	requireFlag("--unshare-pid")
	requireFlag("--unshare-ipc")
	requireFlag("--die-with-parent")
	requireFlag("--new-session")

	// The spec's mounts land under a fresh tmpfs /tmp, so --tmpfs must come
	// before them.
	tmpIdx := slices.Index(args, "--tmpfs")
	for _, m := range spec.Mounts {
		i := slices.Index(args, m.Dst)
		if i < tmpIdx {
			t.Errorf("mount %s must come after --tmpfs /tmp, got args %v", m.Dst, args)
		}
	}

	// "--" separates bwrap flags from the command.
	sep := slices.Index(args, "--")
	if sep < 0 || !slices.Equal(args[sep+1:], spec.Command) {
		t.Errorf("expected command %v after --, got args %v", spec.Command, args)
	}
}

// TestBwrapArgs_SharedNetworkOmitsUnshareNet pins that NetworkShared (the
// opt-in) translates to no --unshare-net, and that NetworkNone remains the
// zero-value default.
func TestBwrapArgs_SharedNetworkOmitsUnshareNet(t *testing.T) {
	spec := SandboxSpec{
		Command: []string{"true"},
		RootFS:  "/",
		Network: NetworkShared,
	}
	if args := bwrapArgs(spec); slices.Contains(args, "--unshare-net") {
		t.Errorf("NetworkShared must not produce --unshare-net, got %v", args)
	}
	if NetworkNone != 0 {
		t.Errorf("NetworkNone must be the zero value (offline by default), got %d", NetworkNone)
	}
}

// TestBwrapArgs_WritableRoot pins that RootWritable produces a read-write bind
// of RootFS at "/" (a chroot package manager must be able to install into the
// rootfs), instead of the read-only bind every other root mode uses.
func TestBwrapArgs_WritableRoot(t *testing.T) {
	spec := SandboxSpec{
		Command: []string{"true"},
		RootFS:  "/rootfs",
		Root:    RootWritable,
	}
	args := bwrapArgs(spec)
	if len(args) < 3 || args[0] != "--bind" || args[1] != "/rootfs" || args[2] != "/" {
		t.Fatalf("expected a writable root bind at \"/\" first, got %v", args)
	}
	if slices.Contains(args, "--ro-bind") {
		t.Errorf("RootWritable must not produce --ro-bind, got %v", args)
	}
	if RootReadOnly != 0 {
		t.Errorf("RootReadOnly must be the zero value (read-only by default), got %d", RootReadOnly)
	}
}

// TestBwrapArgs_OverlayRoot pins that RootOverlay produces an overlay with a
// tmpfs upper: --overlay-src (the read-only lower layer) followed by
// --tmp-overlay / (the ephemeral writable upper), so writes succeed but never
// reach the lower layer.
func TestBwrapArgs_OverlayRoot(t *testing.T) {
	spec := SandboxSpec{
		Command: []string{"true"},
		RootFS:  "/env",
		Root:    RootOverlay,
	}
	args := bwrapArgs(spec)
	if len(args) < 5 || args[0] != "--overlay-src" || args[1] != "/env" || args[2] != "--tmp-overlay" || args[3] != "/" {
		t.Fatalf("expected --overlay-src /env --tmp-overlay / first, got %v", args)
	}
	// The upper layer is a tmpfs, so --tmpfs /tmp (PrivateTmp) is unrelated
	// and must not be conflated with it.
	if slices.Contains(args, "--ro-bind") {
		t.Errorf("RootOverlay must not produce --ro-bind, got %v", args)
	}
}

// TestBwrapArgs_ClearEnv pins that ClearEnv emits --clearenv ahead of the
// --setenv entries, so the sandboxed process starts from an empty environment
// (no inherited credentials) and only the spec's own Env is set.
func TestBwrapArgs_ClearEnv(t *testing.T) {
	spec := SandboxSpec{
		Command:  []string{"true"},
		RootFS:   "/",
		ClearEnv: true,
		Env:      []string{"HOME=/root"},
	}
	args := bwrapArgs(spec)
	clearenv := slices.Index(args, "--clearenv")
	if clearenv < 0 {
		t.Fatalf("expected --clearenv in bwrap args, got %v", args)
	}
	setenv := slices.Index(args, "--setenv")
	if setenv < 0 {
		t.Fatalf("expected --setenv in bwrap args, got %v", args)
	}
	if clearenv > setenv {
		t.Errorf("--clearenv must precede --setenv, got args %v", args)
	}
}

// TestSandbox_LogsNoSandboxedFlag documents a deliberate absence.
//
// Every line this package emits comes from execute, whose whole job is
// wrapping the command in a sandbox. So "sandboxed=true" would be a
// constant restating the function's signature rather than a fact about any
// particular exec — and it reads worse than useless: paired with a command
// like `zypper --root /buildroot install`, it looks self-contradictory,
// because --root is zypper's own package root and not a sandbox at all.
//
// The flag that does carry information is sandboxed=false on rpmpkg/repo's
// runCommand, which genuinely does not wrap. That asymmetry is the point:
// silence where it is the default, an explicit value where it is the
// exception.
func TestSandbox_LogsNoSandboxedFlag(t *testing.T) {
	requireBwrap(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := run(t, context.Background(), t.TempDir(), 30*time.Second, []string{"true"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "msg=exec ") {
		t.Fatalf("expected an exec record, got:\n%s", out)
	}
	if strings.Contains(out, "sandboxed") {
		t.Errorf("this package must not log a sandboxed attribute; got:\n%s", out)
	}
}

// TestSandbox_LogsNoBwrapFlags keeps the sandbox implementation out of the
// log. The bwrap argv is a detail of how confinement is achieved, it changes
// whenever the hardening is retuned, and every exec line at debug verbosity
// would carry a screenful of it. Nothing reads it back: diagnosing a
// confinement problem means reproducing it, not grepping for flags.
//
// The backend's Name() ("bwrap") is still logged — that says which backend
// ran, not how it was configured.
func TestSandbox_LogsNoBwrapFlags(t *testing.T) {
	requireBwrap(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// testSpec contains --unshare-pid and friends; none of them may reach
	// the log.
	if _, err := run(t, context.Background(), t.TempDir(), 30*time.Second, []string{"true"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	out := buf.String()
	for _, forbidden := range []string{"bwrap_args", "--ro-bind", "--unshare-pid", "--unshare-ipc", "--die-with-parent"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("log leaks sandbox implementation detail %q:\n%s", forbidden, out)
		}
	}
	// The command's own arguments are still expected — those are what an
	// operator actually reads the log for.
	if !strings.Contains(out, "cmd=true") {
		t.Errorf("expected the command to still be identified, got:\n%s", out)
	}
	// The backend's name is the one implementation fact deliberately logged.
	if !strings.Contains(out, "sandbox=bwrap") {
		t.Errorf("expected the backend name to be logged, got:\n%s", out)
	}
}

// TestSandbox_LogsCallerLoggerAttributesAndSharedExecID is the test for the
// problem this package's logging actually had. Every exec line used to go
// through slog.Default(), so with two targets configured each line read
//
//	msg="exec done" cmd=zypper sandboxed=true duration=4.2s
//
// and there was no way to tell which target's zypper finished, nor which
// invocation an interleaved "cmd output" line belonged to.
//
// Three properties are asserted, and each is load-bearing:
//
//   - the caller's logger (carrying target=) is used, not slog.Default();
//   - one exec_id is shared by the exec, exec done and cmd output lines, so
//     an invocation's output can be reassembled after the fact;
//   - that id actually differs between two consecutive execs, which is what
//     makes it a correlator rather than a constant.
func TestSandbox_LogsCallerLoggerAttributesAndSharedExecID(t *testing.T) {
	requireBwrap(t)

	var buf bytes.Buffer
	caller := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx := logctx.With(context.Background(), caller.With("target", "fedora/44"))
	for range 2 {
		if _, err := run(t, ctx, t.TempDir(), 30*time.Second,
			[]string{"sh", "-c", "echo hello-from-the-sandbox"}); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 {
		t.Fatal("no log lines captured")
	}

	ids := map[string]int{}
	var startLines, doneLines, outputLines int
	for _, line := range lines {
		// The caller's target= must be on every line, not just some.
		if !strings.Contains(line, "target=fedora/44") {
			t.Errorf("line missing the caller's target attribute: %s", line)
		}

		fields := parseLogFields(line)
		switch fields["msg"] {
		case "exec":
			startLines++
		case "exec done":
			doneLines++
		case "cmd output":
			outputLines++
			if !strings.Contains(fields["line"], "hello-from-the-sandbox") {
				t.Errorf("output line lost its content: %s", line)
			}
		}
		if id := fields["exec_id"]; id != "" {
			ids[id]++
		} else {
			t.Errorf("line missing exec_id: %s", line)
		}
	}

	if startLines != 2 || doneLines != 2 {
		t.Errorf("got %d exec and %d exec-done lines, want 2 each", startLines, doneLines)
	}
	if outputLines == 0 {
		t.Error("no streamed output captured; the echo should have produced cmd output lines")
	}
	if len(ids) != 2 {
		t.Errorf("got %d distinct exec_ids, want 2 (one per exec): %v", len(ids), ids)
	}
	// Each id must appear on the start line, its output and its done line —
	// that shared value is the whole mechanism.
	for id, n := range ids {
		if n < 3 {
			t.Errorf("exec_id=%s appears on only %d lines; want at least exec+output+done", id, n)
		}
	}
}

// parseLogFields extracts key=value pairs from one slog TextHandler line.
// Values may be quoted (the handler quotes any containing a space, so
// msg="exec done" is a single field, not two tokens), so this walks the line
// rather than splitting on whitespace.
func parseLogFields(line string) map[string]string {
	fields := map[string]string{}
	for i := 0; i < len(line); {
		for i < len(line) && line[i] == ' ' {
			i++
		}
		start := i
		for i < len(line) && line[i] != '=' && line[i] != ' ' {
			i++
		}
		if i >= len(line) || line[i] != '=' {
			i = start + 1
			continue
		}
		key := line[start:i]
		i++ // skip '='

		var val string
		if i < len(line) && line[i] == '"' {
			i++
			vs := i
			for i < len(line) && line[i] != '"' {
				i++
			}
			val = line[vs:i]
			if i < len(line) {
				i++ // closing quote
			}
		} else {
			vs := i
			for i < len(line) && line[i] != ' ' {
				i++
			}
			val = line[vs:i]
		}
		fields[key] = val
	}
	return fields
}

// The sandboxed path gets the same outcome line as the direct one: pid, rc
// and outcome, with no error attribute. Both paths previously carried their
// own copy of the same misclassification, so this asserts the sandbox copy
// too rather than trusting the shared one to have been applied here.
func TestSandbox_OutcomeLineShape(t *testing.T) {
	requireBwrap(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := run(t, context.Background(), t.TempDir(), 30*time.Second,
		[]string{"sh", "-c", "exit 3"}); err == nil {
		t.Fatal("want an error from a command that exits non-zero")
	}

	var done string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, `msg="exec done"`) {
			done = line
		}
	}
	if done == "" {
		t.Fatalf("no exec done line:\n%s", buf.String())
	}
	fields := parseLogFields(done)
	if fields["outcome"] != "exit" {
		t.Errorf("outcome = %q, want %q: %s", fields["outcome"], "exit", done)
	}
	if fields["rc"] != "3" {
		t.Errorf("rc = %q, want %q: %s", fields["rc"], "3", done)
	}
	if fields["pid"] == "" {
		t.Errorf("outcome line must carry pid: %s", done)
	}
	if _, ok := fields["error"]; ok {
		t.Errorf("outcome line must not restate the error as an attribute: %s", done)
	}
}

// A deadline that expires must be reported as a timeout, not as a plain
// failure. bwrap answers SIGKILL on an expired context, so the exec error is
// "signal: killed" and carries no hint of the deadline on its own.
func TestSandbox_TimeoutIsReportedAsTimeout(t *testing.T) {
	requireBwrap(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const deadline = 50 * time.Millisecond
	_, err := run(t, context.Background(), t.TempDir(), deadline, []string{"sleep", "30"})
	if err == nil {
		t.Fatal("want an error when the deadline expires")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want it to name the timeout", err)
	}

	var done string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, `msg="exec done"`) {
			done = line
		}
	}
	if done == "" {
		t.Fatalf("no exec done line:\n%s", buf.String())
	}
	fields := parseLogFields(done)
	if fields["outcome"] != "timeout" {
		t.Errorf("outcome = %q, want %q: %s", fields["outcome"], "timeout", done)
	}
	if fields["deadline"] == "" {
		t.Errorf("a timeout must record which ceiling fired: %s", done)
	}
}

func TestSandbox_StructuredLifecycleEventsAndStreams(t *testing.T) {
	requireBwrap(t)

	var buf bytes.Buffer
	caller := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := logctx.With(context.Background(), caller)

	backend := NewBwrap()
	topdir := t.TempDir()
	spec := SandboxSpec{
		Command:    []string{"sh", "-c", "echo out-line && echo err-line >&2"},
		RootFS:     "/",
		Root:       RootReadOnly,
		Mounts:     []Mount{WritableMount(topdir, topdir)},
		PrivateTmp: true,
		Network:    NetworkNone,
		Cwd:        topdir,
		Timeout:    30 * time.Second,
	}

	if _, err := backend.Run(ctx, spec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var sawStart, sawDone, sawStdout, sawStderr bool

	for _, line := range lines {
		fields := parseLogFields(line)
		if fields["component"] != "executor" {
			t.Errorf("expected component=executor on line: %s", line)
		}
		switch fields["event"] {
		case "exec.started":
			sawStart = true
			if fields["cmd"] != "sh" {
				t.Errorf("expected cmd=sh, got %q", fields["cmd"])
			}
		case "exec.completed":
			sawDone = true
			if fields["outcome"] != "ok" || fields["exit_code"] != "0" {
				t.Errorf("unexpected completed fields: %v", fields)
			}
		case "exec.output":
			if fields["stream"] == "stdout" && strings.Contains(fields["line"], "out-line") {
				sawStdout = true
			}
			if fields["stream"] == "stderr" && strings.Contains(fields["line"], "err-line") {
				sawStderr = true
			}
		}
	}

	if !sawStart {
		t.Error("missing exec.started event")
	}
	if !sawDone {
		t.Error("missing exec.completed event")
	}
	if !sawStdout {
		t.Error("missing stdout exec.output event")
	}
	if !sawStderr {
		t.Error("missing stderr exec.output event")
	}
}

func TestSandbox_QuietOutputSuppressesOutputLines(t *testing.T) {
	requireBwrap(t)

	var buf bytes.Buffer
	caller := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := logctx.With(context.Background(), caller)

	backend := NewBwrap()
	topdir := t.TempDir()
	spec := SandboxSpec{
		Command:     []string{"sh", "-c", "echo noisy-out && echo noisy-err >&2"},
		RootFS:      "/",
		Root:        RootReadOnly,
		Mounts:      []Mount{WritableMount(topdir, topdir)},
		PrivateTmp:  true,
		Network:     NetworkNone,
		Cwd:         topdir,
		Timeout:     30 * time.Second,
		QuietOutput: true,
	}

	if _, err := backend.Run(ctx, spec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	for _, line := range lines {
		fields := parseLogFields(line)
		if fields["event"] == "exec.output" || fields["msg"] == "cmd output" {
			t.Errorf("QuietOutput failed to suppress output line: %s", line)
		}
	}
}

func TestSandbox_FailedCommandAttachesStderr(t *testing.T) {
	requireBwrap(t)

	var buf bytes.Buffer
	caller := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := logctx.With(context.Background(), caller)

	backend := NewBwrap()
	topdir := t.TempDir()
	spec := SandboxSpec{
		Command:    []string{"sh", "-c", "echo 'detailed reason for failure' >&2 && exit 7"},
		RootFS:     "/",
		Root:       RootReadOnly,
		Mounts:     []Mount{WritableMount(topdir, topdir)},
		PrivateTmp: true,
		Network:    NetworkNone,
		Cwd:        topdir,
		Timeout:    30 * time.Second,
	}

	_, err := backend.Run(ctx, spec)
	if err == nil {
		t.Fatal("expected error from non-zero exit")
	}

	var doneLine string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(line, `event=exec.completed`) || strings.Contains(line, `msg="exec done"`) {
			doneLine = line
		}
	}
	if doneLine == "" {
		t.Fatalf("missing exec done line in:\n%s", buf.String())
	}

	fields := parseLogFields(doneLine)
	if fields["outcome"] != "exit" {
		t.Errorf("outcome = %q, want exit", fields["outcome"])
	}
	if fields["exit_code"] != "7" {
		t.Errorf("exit_code = %q, want 7", fields["exit_code"])
	}
	if !strings.Contains(fields["stderr"], "detailed reason for failure") {
		t.Errorf("stderr attribute missing failure reason in line: %s", doneLine)
	}
}
