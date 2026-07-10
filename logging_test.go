package main

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// withObservedLogger swaps the process logger for an in-memory observer for the
// duration of a test, returning the observed logs.
func withObservedLogger(t *testing.T) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	prev := logger
	logger = zap.New(core)
	t.Cleanup(func() { logger = prev })
	return logs
}

func TestLogAddsTraceFieldsWhenSpanActive(t *testing.T) {
	logs := withObservedLogger(t)

	tid, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	sid, _ := trace.SpanIDFromHex("0102030405060708")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	log(ctx).Info("hello")

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["trace_id"] != tid.String() {
		t.Fatalf("trace_id = %v, want %s", fields["trace_id"], tid.String())
	}
	if fields["span_id"] != sid.String() {
		t.Fatalf("span_id = %v, want %s", fields["span_id"], sid.String())
	}
}

func TestLogOmitsTraceFieldsWithoutSpan(t *testing.T) {
	logs := withObservedLogger(t)

	log(context.Background()).Info("hello")

	fields := logs.All()[0].ContextMap()
	if _, ok := fields["trace_id"]; ok {
		t.Fatal("trace_id should be absent without an active span")
	}
	if _, ok := fields["span_id"]; ok {
		t.Fatal("span_id should be absent without an active span")
	}
}
