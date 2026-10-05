package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jirib/golang-jirib-utils/httpapi"
	"github.com/jirib/golang-jirib-utils/metrics"
	"github.com/jirib/golang-jirib-utils/tracing"
)

type echoRequest struct {
	Message string `json:"message"`
}

type echoResponse struct {
	Reply   string `json:"reply"`
	TraceID string `json:"trace_id"`
}

// TestHTTPAPI_WithAuthMetricsAndTracing verifies the integration of httpapi.Server,
// bearer authentication, tracing span recording, and metrics collection.
func TestHTTPAPI_WithAuthMetricsAndTracing(t *testing.T) {
	// Allocate a random port on localhost
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on localhost: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	tracer := tracing.New("integration-test-tracer")
	const authToken = "secret-token-12345"

	srv := httpapi.New(addr, nil)
	srv.Token = authToken
	srv.Realm = "integration-tests"

	// Register an instrumented endpoint
	srv.Handle(http.MethodPost, "/api/v1/echo", func(ctx context.Context, payload []byte) (any, error) {
		ctx, span := tracer.StartSpan(ctx, "http.echo")
		defer span.End()

		var req echoRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			metrics.IncCall("http_echo", "error")
			return nil, fmt.Errorf("invalid json payload: %w", err)
		}

		metrics.IncCall("http_echo", "ok")
		metrics.ObserveDuration("http_echo", 5*time.Millisecond)

		return echoResponse{
			Reply:   "ack: " + req.Message,
			TraceID: tracing.TraceID(ctx),
		}, nil
	})

	serverCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- srv.Serve(serverCtx)
	}()

	// Wait briefly for server to bind
	time.Sleep(100 * time.Millisecond)

	baseURL := fmt.Sprintf("http://%s/api/v1/echo", addr)

	t.Run("UnauthorizedRequestRejected", func(t *testing.T) {
		body := bytes.NewReader([]byte(`{"message":"unauth"}`))
		req, err := http.NewRequest(http.MethodPost, baseURL, body)
		if err != nil {
			t.Fatal(err)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected status 401 Unauthorized, got: %d", resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="integration-tests"` {
			t.Errorf("unexpected WWW-Authenticate header: %q", got)
		}
	})

	t.Run("AuthorizedRequestSucceeds", func(t *testing.T) {
		body := bytes.NewReader([]byte(`{"message":"hello-mcp"}`))
		req, err := http.NewRequest(http.MethodPost, baseURL, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+authToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected status 200 OK, got %d: %s", resp.StatusCode, string(b))
		}

		var res echoResponse
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}

		if res.Reply != "ack: hello-mcp" {
			t.Errorf("got reply %q, want %q", res.Reply, "ack: hello-mcp")
		}
		if res.TraceID == "" {
			t.Error("expected non-empty trace ID in response")
		}
	})

	t.Run("MetricsAndTraceSpansRecorded", func(t *testing.T) {
		// Verify OpenMetrics encoding includes our metric
		var metricsBuf bytes.Buffer
		if err := metrics.EncodeOpenMetrics(&metricsBuf); err != nil {
			t.Fatalf("EncodeOpenMetrics failed: %v", err)
		}
		metricsOut := metricsBuf.String()
		if !strings.Contains(metricsOut, "mcp_tool_calls_total") {
			t.Errorf("OpenMetrics output missing mcp_tool_calls_total:\n%s", metricsOut)
		}

		// Verify traces can be drained
		traceJSON, err := tracer.Drain()
		if err != nil {
			t.Fatalf("tracer.Drain() failed: %v", err)
		}
		if len(traceJSON) == 0 {
			t.Error("expected drained trace JSON to be non-empty")
		}
		if !strings.Contains(string(traceJSON), "http.echo") {
			t.Errorf("trace JSON does not mention span 'http.echo':\n%s", string(traceJSON))
		}
	})

	cancelServer()
	<-serverErrCh
}
