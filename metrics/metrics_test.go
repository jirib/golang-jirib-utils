package metrics

import (
	"bytes"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestDefaultRecorder(t *testing.T) {
	ToolCalls.Reset()
	ToolDuration.Reset()

	DefaultRecorder.IncCall("example_tool", "ok")
	DefaultRecorder.IncCall("example_tool", "error")

	if got := testutil.ToFloat64(ToolCalls.WithLabelValues("example_tool", "ok")); got != 1 {
		t.Fatalf("ToolCalls{example_tool,ok} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(ToolCalls.WithLabelValues("example_tool", "error")); got != 1 {
		t.Fatalf("ToolCalls{example_tool,error} = %v, want 1", got)
	}

	// CollectAndCount on a *HistogramVec counts child histograms (one per
	// label combination seen), not raw observations.
	DefaultRecorder.ObserveDuration("example_tool", 0)
	if n := testutil.CollectAndCount(ToolDuration); n != 1 {
		t.Fatalf("ToolDuration series count = %d, want 1", n)
	}
}

// EncodeOpenMetrics must produce the standard OpenMetrics text, terminated by
// the "# EOF" line the format requires.
func TestEncodeOpenMetricsEndsWithEOF(t *testing.T) {
	ToolCalls.Reset()
	ToolCalls.WithLabelValues("example_tool", "ok").Inc()

	var buf bytes.Buffer
	if err := EncodeOpenMetrics(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "mcp_tool_calls_total") {
		t.Errorf("output missing the registered metric:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "# EOF") {
		t.Errorf("output must end with the OpenMetrics # EOF marker:\n%s", out)
	}
}
