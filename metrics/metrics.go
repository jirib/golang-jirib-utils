// Package metrics provides Prometheus instrumentation for MCP server tool calls.
package metrics

import (
	"io"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

var (
	ToolCalls = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mcp_tool_calls_total",
			Help: "MCP tool invocations, by tool and outcome.",
		},
		[]string{"tool", "status"},
	)

	ToolDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "mcp_tool_duration_seconds",
			Help:    "MCP tool call latency, by tool.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 14), // ~10ms .. ~80s
		},
		[]string{"tool"},
	)
)

func init() {
	prometheus.MustRegister(ToolCalls, ToolDuration)
}

// ObserveDuration records tool call latency.
func ObserveDuration(tool string, d time.Duration) {
	ToolDuration.WithLabelValues(tool).Observe(d.Seconds())
}

// IncCall increments the invocation counter for tool with the given status ("ok" or "error").
func IncCall(tool, status string) {
	ToolCalls.WithLabelValues(tool, status).Inc()
}

// Recorder abstracts metric recording for tool handlers.
type Recorder interface {
	ObserveDuration(tool string, d time.Duration)
	IncCall(tool, status string)
}

type defaultRecorder struct{}

func (defaultRecorder) ObserveDuration(tool string, d time.Duration) { ObserveDuration(tool, d) }
func (defaultRecorder) IncCall(tool, status string)                  { IncCall(tool, status) }

// DefaultRecorder records metrics using ToolCalls and ToolDuration.
var DefaultRecorder Recorder = defaultRecorder{}

// EncodeOpenMetrics writes registered Prometheus metrics to w in OpenMetrics format ending with "# EOF".
func EncodeOpenMetrics(w io.Writer) error {
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return err
	}
	enc := expfmt.NewEncoder(w, expfmt.FmtOpenMetrics_1_0_0)
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			return err
		}
	}
	_, err = expfmt.FinalizeOpenMetrics(w)
	return err
}
