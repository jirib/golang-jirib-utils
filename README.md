# golang-jirib-utils

A collection of foundational, idiomatic Go packages designed for daemons, command-line control tools, process isolation, Model Context Protocol (MCP) servers, and observability.

Originally extracted from system services such as [`pkg-brokerd`](https://github.com/jirib/pkg-brokerd), these packages eliminate recurring boilerplate around Unix domain socket IPC, Bubblewrap sandboxing, structured logging with RFC3339 microsecond timestamps, in-memory OpenTelemetry tracing, and Prometheus metrics.

---

## Package Overview

| Package | Purpose | Why It Exists |
|---|---|---|
| [`config`](#config) | Type-safe environment variable parsing | Validates positive durations and booleans with helpful error messages. |
| [`ctl`](#ctl) | Unix domain socket IPC & CLI dispatcher | Robust daemon control plane with framing protocol, client, server, and CLI subcommands. |
| [`execout`](#execout) | Process outcome classification & formatting | Classifies exits, timeouts, and cancellations into structured slog attributes and wrapped errors. |
| [`httpapi`](#httpapi) | JSON HTTP API server | Bounded payload sizes, bearer auth, automatic `request_id` correlation, and graceful context-aware shutdown. |
| [`logctx`](#logctx) | Context-aware structured logging | Injects/extracts `*slog.Logger` in `context.Context` and correlates logs via a 3-tier causal hierarchy (`request_id`, `operation_id`, `exec_id`). |
| [`logging`](#logging) | Uniform slog configuration | Provides microsecond RFC3339 timestamps and case-insensitive log level parsing. |
| [`mcpserver/auth`](#mcpserverauth) | HTTP Bearer token authentication | Constant-time bearer token middleware with `WWW-Authenticate` challenge headers. |
| [`mcpserver/tools`](#mcpservertools) | Model Context Protocol tool helpers | Generates JSON schemas via reflection and instruments MCP tool handlers with metrics and tracing. |
| [`metrics`](#metrics) | Prometheus instrumentation & OpenMetrics | Tracks tool call latency and outcomes, with an OpenMetrics text format exporter. |
| [`sandbox`](#sandbox) | Bubblewrap (`bwrap`) isolation runner | Executes commands inside unprivileged Linux sandboxes with private mounts, tmpfs, and network isolation. |
| [`tracing`](#tracing) | In-memory OpenTelemetry tracer | Captures OTLP traces into a bounded ring buffer for on-demand JSON draining without an external collector. |

---

## Directory Reference & Usage

### `config`

#### What It Provides
- `config.PositiveDurationFromEnv(key string, def time.Duration) (time.Duration, error)`
- `config.BoolFromEnv(key string, def bool) (bool, error)`

#### Why It Exists
Daemons and CLI utilities frequently configure operational timeouts (such as idle session garbage collection or shutdown grace periods) and feature flags from environment variables. Standard `os.Getenv` requires repetitive validation and error formatting. `config` ensures that durations are strictly positive and booleans accept standard representations (`1`, `true`, `0`, `false`), returning descriptive errors when configuration is malformed.

#### Usage Example
```go
package main

import (
	"fmt"
	"log"
	"time"

	"github.com/jirib/golang-jirib-utils/config"
)

func main() {
	gcInterval, err := config.PositiveDurationFromEnv("GC_INTERVAL", 10*time.Minute)
	if err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	debugMode, err := config.BoolFromEnv("DEBUG_MODE", false)
	if err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	fmt.Printf("Configured GC: %v, Debug: %v\n", gcInterval, debugMode)
}
```

---

### `ctl`

#### What It Provides
- **Wire Framing Protocol (`proto.go`)**: Fixed 8-byte framing header (version, type, length) bounding payloads to 8 MiB.
- **Server (`server.go`)**: Unix domain socket listener with 0600 file modes, stale socket detection/cleanup, and graceful shutdown.
- **Client (`client.go`)**: Context-aware client issuing requests and decoding JSON responses or typed errors.
- **CLI Dispatcher (`cli.go`)**: Subcommand dispatcher supporting `--socket` flags, help generation, and error formatting.
- **Path Resolution (`path.go`)**: Resolves socket paths via `$XDG_RUNTIME_DIR`, `/run/user/<uid>`, or fallback temporary directories.

#### Why It Exists
Writing a dedicated administrative control socket for a background daemon usually entails substantial low-level socket management: handling stale socket files left behind by ungraceful crashes, enforcing file permissions to avoid multi-user security holes, framing messages over a stream socket, and implementing client-side timeout deadlines. The `ctl` package provides an end-to-end solution for daemon control channels.

#### Usage Example

**Server:**
```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/jirib/golang-jirib-utils/ctl"
)

const CommandStatus ctl.Type = 1

func main() {
	srv := &ctl.Server{
		App: "my-daemon",
		Handler: func(ctx context.Context, typ ctl.Type, payload []byte) ([]byte, error) {
			switch typ {
			case CommandStatus:
				return json.Marshal(map[string]string{"status": "running"})
			default:
				return nil, fmt.Errorf("unsupported command type: %d", typ)
			}
		},
	}

	if err := srv.Listen(); err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	log.Printf("Listening on %s", srv.SocketPath())
	if err := srv.Serve(); err != nil && err != ctl.ErrClosed {
		log.Fatal(err)
	}
}
```

**Client:**
```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jirib/golang-jirib-utils/ctl"
)

const CommandStatus ctl.Type = 1

func main() {
	client := &ctl.Client{
		App:     "my-daemon",
		Timeout: 5 * time.Second,
	}

	var resp struct {
		Status string `json:"status"`
	}

	if err := client.Call(context.Background(), CommandStatus, nil, &resp); err != nil {
		log.Fatalf("control call failed: %v", err)
	}

	fmt.Printf("Daemon status: %s\n", resp.Status)
}
```

---

### `execout`

#### What It Provides
- `Outcome`: Enum indicating how a process ended (`OutcomeOK`, `OutcomeExit`, `OutcomeTimeout`, `OutcomeCancel`, `OutcomeStart`).
- `Classify(ctxErr, runErr, ps)`: Examines context errors and `os.ProcessState` to determine outcome, PID, exit code, and terminating signal.
- `(Result).Attrs(duration, deadline, base...)`: Formats structured key-value attributes for `slog`.
- `(Result).Wrap(prefix, deadline, runErr)`: Wraps exec errors with contextual explanations while preserving error unwrapping.

#### Why It Exists
Go's standard `os/exec` terminates processes upon context cancellation or timeout using `SIGKILL`. Consequently, the error returned is typically an opaque `"signal: killed"`, hiding whether the termination was caused by a deadline expiration, an explicit caller cancellation, or an external signal. `execout` reconciles `context` errors with process exit states to produce actionable logs and errors.

#### Usage Example
```go
package main

import (
	"context"
	"log/slog"
	"os/exec"
	"time"

	"github.com/jirib/golang-jirib-utils/execout"
)

func runCommand(ctx context.Context, timeout time.Duration, name string, args ...string) error {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, name, args...)
	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	outcome := execout.Classify(runCtx.Err(), runErr, cmd.ProcessState)
	slog.Info("process finished", outcome.Attrs(elapsed, timeout, "binary", name)...)

	if runErr != nil {
		return outcome.Wrap("command execution", timeout, runErr)
	}
	return nil
}
```

---

### `httpapi`

#### What It Provides
- `Server`: HTTP server routing JSON command handlers with optional bearer token authentication.
- Request body bounding: limits request sizes to 1 MiB (`maxBody`) to prevent memory exhaustion.
- Uniform JSON error responses and status code serialization.
- Context-driven graceful shutdown with configurable header timeouts.

#### Why It Exists
When exposing internal daemon functions or MCP tools over HTTP, developers need consistent request payload limiting, authentication enforcement, structured error serialization, and clean server lifecycles without pulling in heavyweight web frameworks.

#### Usage Example
```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/jirib/golang-jirib-utils/httpapi"
)

type GreetRequest struct {
	Name string `json:"name"`
}

func main() {
	srv := httpapi.New("127.0.0.1:8080", nil)
	srv.Token = "super-secret-key"
	srv.Realm = "admin-api"

	srv.Handle(http.MethodPost, "/v1/greet", func(ctx context.Context, payload []byte) (any, error) {
		var req GreetRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		if req.Name == "" {
			return nil, errors.New("name is required")
		}
		return map[string]string{"greeting": "Hello, " + req.Name}, nil
	})

	log.Fatal(srv.Serve(context.Background()))
}
```

---

### `logctx`

#### What It Provides
- `With(ctx context.Context, logger *slog.Logger) context.Context`: Injects an `*slog.Logger` into context.
- `From(ctx context.Context) *slog.Logger`: Retrieves the contextual logger, falling back to `slog.Default()`.
- `NewID() string`: Generates a random 6-character hex identifier.
- `WithRequestID(ctx context.Context, id string) context.Context`: Attaches `request_id` to context and contextual logger.
- `RequestID(ctx context.Context) string`: Extracts `request_id` from context.
- `WithOperationID(ctx context.Context, id string) context.Context`: Attaches `operation_id` to context and contextual logger.
- `OperationID(ctx context.Context) string`: Extracts `operation_id` from context.
- `WithExecID(ctx context.Context, id string) context.Context`: Attaches `exec_id` to context and contextual logger.
- `ExecID(ctx context.Context) string`: Extracts `exec_id` from context.
- `WithAttrs(ctx context.Context, attrs ...any) context.Context`: Decorates contextual logger with arbitrary domain attributes (`target`, `package`, etc.).
- `FromWithID(ctx context.Context, id string) *slog.Logger`: Retrieves the logger with an attached `exec_id` attribute.

#### Why It Exists
In concurrent services executing multiple tasks or background commands simultaneously, log lines from different workers easily interleave. Passing the logger and correlation IDs via `context.Context` ensures that contextual attributes automatically follow all downstream function calls without polluting function signatures:
- **`request_id`**: Identifies boundary requests (e.g. HTTP, CLI, MCP).
- **`operation_id`**: Identifies business operations (e.g. package prep, cache refresh).
- **`exec_id`**: Identifies individual subprocess runs (e.g. sandboxed `zypper` or `rpm` executions).

#### Usage Example
```go
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/jirib/golang-jirib-utils/logctx"
)

func runStep(ctx context.Context) {
	// Automatically includes request_id, operation_id, target, and exec_id
	log := logctx.From(ctx)
	log.Info("step completed", "outcome", "ok")
}

func main() {
	baseLogger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx := logctx.With(context.Background(), baseLogger)

	// 1. Boundary: attach request_id
	ctx = logctx.WithRequestID(ctx, "8f31")

	// 2. Domain operation: attach operation_id and domain attributes
	ctx = logctx.WithOperationID(ctx, "op-91ac")
	ctx = logctx.WithAttrs(ctx, "target", "opensuse/tumbleweed", "package", "kernel-default")

	// 3. Execution: attach exec_id
	taskCtx := logctx.WithExecID(ctx, "3144d2")

	runStep(taskCtx)
}
```

---

### `logging`

#### What It Provides
- `TimeFormat`: RFC3339 with microsecond precision and local timezone offset (`"2006-01-02T15:04:05.000000Z07:00"`).
- `FormatTime`: `slog.Attr` replacement function applying microsecond formatting.
- `NewHandler(w io.Writer, level slog.Level) slog.Handler`: Constructs an `slog.TextHandler` configured with microsecond timestamps.
- `NewLogger(level slog.Level) *slog.Logger`: Constructs a standard stderr logger.
- `ParseLevel(name, value string) (slog.Level, error)`: Parses log levels (`debug`, `info`, `warn`, `error`) case-insensitively.

#### Why It Exists
Default `slog` text handlers omit sub-second time resolution, which makes debugging rapid sequential events or measuring microsecond process operations difficult. `logging` standardizes log emission across all binaries with microsecond precision and clear level parsing.

#### Usage Example
```go
package main

import (
	"log/slog"

	"github.com/jirib/golang-jirib-utils/logging"
)

func main() {
	level, err := logging.ParseLevel("LOG_LEVEL", "debug")
	if err != nil {
		level = slog.LevelInfo
	}

	logger := logging.NewLogger(level)
	slog.SetDefault(logger)

	slog.Debug("system initialized", "subsystem", "daemon")
}
```

---

### `mcpserver/auth`

#### What It Provides
- `BearerMiddleware(token, realm string, next http.Handler) http.Handler`: Validates `Authorization: Bearer <token>` using constant-time string comparison (`crypto/subtle.ConstantTimeCompare`).
- Sends `WWW-Authenticate: Bearer realm="<realm>"` headers on authentication failure.

#### Why It Exists
Protecting Model Context Protocol (MCP) or management HTTP endpoints against unauthorized requests and timing side-channel attacks requires constant-time token comparison and standard HTTP challenge headers.

#### Usage Example
```go
package main

import (
	"net/http"

	"github.com/jirib/golang-jirib-utils/mcpserver/auth"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/tools", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tools": []}`))
	})

	securedMux := auth.BearerMiddleware("secret-auth-token", "mcp-service", mux)
	http.ListenAndServe(":8080", securedMux)
}
```

---

### `mcpserver/tools`

#### What It Provides
- `InputSchema[In any](descriptions map[string]string) *jsonschema.Schema`: Derives a JSON schema from Go structs with property descriptions using `github.com/google/jsonschema-go`.
- `WithEnum(schema *jsonschema.Schema, property string, values ...string) *jsonschema.Schema`: Adds enum constraints to a schema property.
- `Instrumented[In any](tracer *tracing.Tracer, rec metrics.Recorder, tool string, h HandlerFunc[In]) HandlerFunc[In]`: Automatically wraps MCP tool handlers with OpenTelemetry tracing spans and Prometheus duration/counter metrics.

#### Why It Exists
MCP tools require strict JSON schemas describing their input parameters and benefit greatly from telemetry to monitor latency and failure rates. Handcrafting JSON schemas or writing boilerplate metric/tracing wrappers for each tool is error-prone; this package generates schemas directly from types and standardizes telemetry.

#### Usage Example
```go
package main

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/jirib/golang-jirib-utils/mcpserver/tools"
	"github.com/jirib/golang-jirib-utils/metrics"
	"github.com/jirib/golang-jirib-utils/tracing"
)

type BuildInput struct {
	Target string `json:"target"`
	Arch   string `json:"arch"`
}

func main() {
	schema := tools.InputSchema[BuildInput](map[string]string{
		"target": "Operating system distribution target",
		"arch":   "Target CPU architecture",
	})
	tools.WithEnum(schema, "arch", "x86_64", "aarch64")

	tracer := tracing.New("build-service")
	handler := tools.Instrumented(tracer, metrics.DefaultRecorder, "build", func(
		ctx context.Context,
		req *mcp.CallToolRequest,
		in BuildInput,
	) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, "success", nil
	})

	_ = handler
}
```

---

### `metrics`

#### What It Provides
- `ToolCalls`: Prometheus `CounterVec` (`mcp_tool_calls_total`) labeled by `tool` and `status` (`ok`, `error`).
- `ToolDuration`: Prometheus `HistogramVec` (`mcp_tool_duration_seconds`) labeled by `tool` with exponential buckets (~10ms to ~80s).
- `Recorder` interface and `DefaultRecorder` for testing and custom metric dispatch.
- `EncodeOpenMetrics(w io.Writer) error`: Serializes registered Prometheus metrics in OpenMetrics text format ending with `# EOF`.

#### Why It Exists
Provides out-of-the-box Prometheus metrics tracking invocation counts and latency distributions for services and MCP tools, with standardized OpenMetrics export.

#### Usage Example
```go
package main

import (
	"net/http"
	"time"

	"github.com/jirib/golang-jirib-utils/metrics"
)

func main() {
	// Record metrics
	metrics.IncCall("compile", "ok")
	metrics.ObserveDuration("compile", 250*time.Millisecond)

	// Expose OpenMetrics endpoint
	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/openmetrics-text; version=1.0.0; charset=utf-8")
		if err := metrics.EncodeOpenMetrics(w); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	http.ListenAndServe(":9090", nil)
}
```

---

### `sandbox`

#### What It Provides
- `SandboxSpec`: Declares execution constraints: `Command`, `RootFS`, `RootMode` (`RootReadOnly`, `RootWritable`, `RootOverlay`), `Mounts`, `PrivateTmp`, `Network` (`NetworkNone`, `NetworkShared`), `Env`, `ClearEnv`, `Cwd`, and `Timeout`.
- `BwrapBackend`: Confinement backend translating specifications into Bubblewrap (`bwrap`) CLI arguments (`--unshare-net`, `--unshare-pid`, `--unshare-ipc`, `--die-with-parent`, `--new-session`, mount bindings).
- Real-time debug output streaming with byte-capped capture buffers (up to 10 MiB) to prevent memory exhaustion.
- Automatic integration with `execout` classification and `logctx` execution ID tracking.

#### Why It Exists
Executing untrusted or host-modifying tools (such as RPM package managers `zypper` or `dnf`) requires strict process and filesystem isolation. Running container engines like Docker or Podman within daemons introduces significant overhead, daemon dependencies, and complex nesting issues. Bubblewrap provides unprivileged, lightweight sandboxing directly on modern Linux kernels.

#### Usage Example
```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jirib/golang-jirib-utils/sandbox"
)

func main() {
	spec := sandbox.SandboxSpec{
		Command:    []string{"echo", "running inside bwrap"},
		RootFS:     "/",
		Root:       sandbox.RootReadOnly,
		Mounts:     []sandbox.Mount{sandbox.ReadOnlyMount("/etc", "/etc")},
		PrivateTmp: true,
		Network:    sandbox.NetworkNone,
		Timeout:    10 * time.Second,
	}

	backend := sandbox.NewBwrap()
	res, err := backend.Run(context.Background(), spec)
	if err != nil {
		log.Fatalf("sandbox execution failed: %v", err)
	}

	fmt.Printf("Output: %s\n", res.Stdout)
}
```

---

### `tracing`

#### What It Provides
- `Tracer`: Wraps OpenTelemetry's `trace.Tracer` and exports spans to an in-memory thread-safe `spanRing`.
- Configurable ring buffer capacity (`WithBufferSize`, default: 10,000 spans).
- `StartSpan(ctx, name, attrs...)`: Starts a span and returns updated context.
- `TraceID(ctx)`: Extracts active trace ID hex string from context.
- `Drain()`: Drains completed spans and serializes them to OTLP JSON format (`ExportTraceServiceRequest`).

#### Why It Exists
Distributed tracing typically requires deploying and running an external OpenTelemetry collector daemon or sidecar. In standalone CLI utilities, local developer daemons, or offline tools, hosting an external collector is unnecessary overhead. `tracing` keeps spans in a bounded in-memory buffer, allowing callers to inspect, log, or export trace data over HTTP/control endpoints on demand.

#### Usage Example
```go
package main

import (
	"context"
	"fmt"
	"log"

	"go.opentelemetry.io/otel/attribute"
	"github.com/jirib/golang-jirib-utils/tracing"
)

func main() {
	t := tracing.New("worker-service", tracing.WithBufferSize(5000))

	ctx, span := t.StartSpan(context.Background(), "process-job", attribute.String("job_id", "123"))
	span.End()

	fmt.Printf("Recorded Trace ID: %s\n", tracing.TraceID(ctx))

	// Export captured spans
	jsonData, err := t.Drain()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Exported OTLP JSON: %s\n", string(jsonData))
}
```

---

## Development & Testing

This project uses [mise](https://mise.jdx.dev/) for hermetic tool management and task execution.

### Prerequisites
- [mise](https://mise.jdx.dev/) installed
- Linux kernel with user namespace support
- `bubblewrap` (`bwrap`) installed (optional for unit tests, required for sandbox integration tests)

### Available Tasks
Run `mise tasks` to view available tasks:
- **`mise run test`**: Builds, vets, and runs race-detected unit tests across all packages:
  ```bash
  mise run test
  ```
- **`mise run test-integration`**: Runs cross-package end-to-end integration tests under `tests/integration/`:
  ```bash
  mise run test-integration
  ```
- **`mise run clean`**: Cleans Go test caches and removes temporary test artifacts:
  ```bash
  mise run clean
  ```

### Continuous Integration
GitHub Actions runs all tests on every push and pull request against `main` / `master` using `jdx/mise-action` with `bubblewrap` installed in Ubuntu runners. See `.github/workflows/ci.yaml`.

---

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details.
