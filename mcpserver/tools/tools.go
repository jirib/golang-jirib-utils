// Package tools provides helpers for registering MCP tools with schema generation and instrumentation.
package tools

import (
	"context"
	"reflect"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/jirib/golang-jirib-utils/metrics"
	"github.com/jirib/golang-jirib-utils/tracing"
)

// InputSchema builds a JSON schema for In, annotating properties with descriptions from the map.
func InputSchema[In any](descriptions map[string]string) *jsonschema.Schema {
	s, err := jsonschema.ForType(reflect.TypeFor[In](), &jsonschema.ForOptions{})
	if err != nil {
		panic(err)
	}
	for prop, desc := range descriptions {
		if p, ok := s.Properties[prop]; ok {
			p.Description = desc
		}
	}
	return s
}

// WithEnum attaches allowed enum values to property in schema and returns the modified schema.
func WithEnum(schema *jsonschema.Schema, property string, values ...string) *jsonschema.Schema {
	if schema != nil && schema.Properties != nil {
		if p, ok := schema.Properties[property]; ok {
			p.Enum = make([]any, len(values))
			for i, v := range values {
				p.Enum[i] = v
			}
		}
	}
	return schema
}

// HandlerFunc is the function signature for an MCP tool handler.
type HandlerFunc[In any] func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error)

// Instrumented wraps an MCP tool handler with tracing and metrics recording.
func Instrumented[In any](tracer *tracing.Tracer, rec metrics.Recorder, tool string, h HandlerFunc[In]) HandlerFunc[In] {
	if rec == nil {
		rec = metrics.DefaultRecorder
	}
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		ctx, span := tracer.StartSpan(ctx, "mcp.tool/"+tool, attribute.String("mcp.tool", tool))
		defer span.End()

		start := time.Now()
		res, out, err := h(ctx, req, in)
		rec.ObserveDuration(tool, time.Since(start))
		status := "ok"
		if err != nil || (res != nil && res.IsError) {
			status = "error"
			span.SetStatus(codes.Error, "tool call failed")
		}
		span.SetAttributes(attribute.String("mcp.status", status))
		rec.IncCall(tool, status)
		return res, out, err
	}
}
