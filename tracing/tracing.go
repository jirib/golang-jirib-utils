// Package tracing provides in-memory OpenTelemetry tracing with a bounded ring buffer.
package tracing

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// DefaultBufferSize is the default ring buffer capacity for completed spans.
const DefaultBufferSize = 10_000

// Tracer manages trace instrumentation and buffers completed spans.
type Tracer struct {
	tracer trace.Tracer
	ring   *spanRing
}

type config struct {
	bufferSize int
}

// Option configures a Tracer.
type Option func(*config)

// WithBufferSize sets the ring buffer capacity.
func WithBufferSize(n int) Option {
	return func(c *config) { c.bufferSize = n }
}

// New constructs a Tracer and registers its provider globally.
func New(instrumentationName string, options ...Option) *Tracer {
	cfg := config{bufferSize: DefaultBufferSize}
	for _, o := range options {
		o(&cfg)
	}
	ring := newSpanRing(cfg.bufferSize)

	exporter, err := otlptrace.New(context.Background(), &bufferClient{ring: ring})
	if err != nil {
		return &Tracer{tracer: otel.Tracer(instrumentationName), ring: ring}
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)),
	)
	otel.SetTracerProvider(tp)

	return &Tracer{tracer: tp.Tracer(instrumentationName), ring: ring}
}

// StartSpan starts a span and returns the updated context and span handle.
func (t *Tracer) StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return t.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}

// TraceID extracts the active trace ID from ctx, or returns an empty string if none.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// Drain returns the buffered spans serialized as OTLP JSON and clears the buffer.
func (t *Tracer) Drain() ([]byte, error) {
	req := &coltracepb.ExportTraceServiceRequest{ResourceSpans: t.ring.drain()}
	return protojson.Marshal(req)
}

type bufferClient struct {
	ring *spanRing
}

func (c *bufferClient) Start(ctx context.Context) error { return nil }
func (c *bufferClient) Stop(ctx context.Context) error  { return nil }

func (c *bufferClient) UploadTraces(ctx context.Context, protoSpans []*tracepb.ResourceSpans) error {
	c.ring.add(protoSpans)
	return nil
}

// spanRing is a thread-safe ring buffer storing *tracepb.ResourceSpans.
type spanRing struct {
	mu   sync.Mutex
	buf  []*tracepb.ResourceSpans
	head int
	size int
}

func newSpanRing(capacity int) *spanRing {
	if capacity <= 0 {
		capacity = DefaultBufferSize
	}
	return &spanRing{buf: make([]*tracepb.ResourceSpans, capacity)}
}

func (r *spanRing) add(spans []*tracepb.ResourceSpans) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range spans {
		idx := (r.head + r.size) % len(r.buf)
		r.buf[idx] = s
		if r.size < len(r.buf) {
			r.size++
		} else {
			r.head = (r.head + 1) % len(r.buf) // overwrite oldest
		}
	}
}

// drain returns buffered spans in FIFO order and zeroes the buffer.
func (r *spanRing) drain() []*tracepb.ResourceSpans {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*tracepb.ResourceSpans, 0, r.size)
	for i := 0; i < r.size; i++ {
		out = append(out, r.buf[(r.head+i)%len(r.buf)])
	}
	clear(r.buf)
	r.head, r.size = 0, 0
	return out
}
