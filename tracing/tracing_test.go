package tracing

import (
	"context"
	"testing"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestSpanRingIsBounded(t *testing.T) {
	ring := newSpanRing(2)
	ring.add([]*tracepb.ResourceSpans{{SchemaUrl: "one"}})
	ring.add([]*tracepb.ResourceSpans{{SchemaUrl: "two"}})
	ring.add([]*tracepb.ResourceSpans{{SchemaUrl: "three"}})

	got := ring.drain()
	if len(got) != 2 {
		t.Fatalf("drained %d entries, want 2 (bounded)", len(got))
	}
	// The oldest ("one") is discarded; the rest survive in order.
	if got[0].SchemaUrl != "two" || got[1].SchemaUrl != "three" {
		t.Errorf("drained %q, %q; want two, three", got[0].SchemaUrl, got[1].SchemaUrl)
	}
}

func TestTracerRecordsAndDrainsSpans(t *testing.T) {
	tr := New("test/tracing", WithBufferSize(4))

	for _, name := range []string{"first-span", "second-span"} {
		_, span := tr.StartSpan(context.Background(), name)
		span.End()
	}

	data, err := tr.Drain()
	if err != nil {
		t.Fatal(err)
	}

	var req coltracepb.ExportTraceServiceRequest
	if err := protojson.Unmarshal(data, &req); err != nil {
		t.Fatalf("drain is not valid OTLP JSON (%v): %s", err, data)
	}
	var names []string
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				names = append(names, sp.Name)
			}
		}
	}
	if len(names) != 2 || names[0] != "first-span" || names[1] != "second-span" {
		t.Errorf("drained spans = %v, want [first-span second-span]", names)
	}

	// Drain clears the buffer: a second drain is empty.
	again, err := tr.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != "{}" {
		t.Errorf("second drain = %s, want empty", again)
	}
}
