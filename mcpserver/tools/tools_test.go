package tools

import (
	"context"
	"testing"

	"github.com/jirib/golang-jirib-utils/tracing"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type sampleInput struct {
	Name  string `json:"name"`
	Count int    `json:"count,omitempty"`
}

func TestInputSchemaAndWithEnum(t *testing.T) {
	s := InputSchema[sampleInput](map[string]string{
		"name": "the name",
	})
	if s == nil || s.Properties == nil {
		t.Fatal("expected schema with properties")
	}
	if s.Properties["name"] == nil || s.Properties["name"].Description != "the name" {
		t.Errorf("expected property 'name' description 'the name', got %v", s.Properties["name"])
	}

	WithEnum(s, "name", "val1", "val2")
	if len(s.Properties["name"].Enum) != 2 {
		t.Errorf("expected 2 enum values, got %v", s.Properties["name"].Enum)
	}
}

func TestInstrumented(t *testing.T) {
	tr := tracing.New("test/tools")
	h := Instrumented[sampleInput](tr, nil, "sample", func(ctx context.Context, req *mcp.CallToolRequest, in sampleInput) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, "result", nil
	})

	res, out, err := h(context.Background(), &mcp.CallToolRequest{}, sampleInput{Name: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || out != "result" {
		t.Errorf("unexpected output: %v, %v", res, out)
	}
}
