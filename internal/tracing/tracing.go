// Package tracing generates W3C traceparent header values so the client can
// hand Core a trace ID to root its OpenTelemetry trace on, without pulling in
// the OTel SDK itself (client#64, companion to core#80). Core's span-naming
// and attribute conventions are still settling, so full instrumentation
// (spans for watcher/queue/uploader, an SDK + exporter) is deliberately out
// of scope here — this is just the header format.
package tracing

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewTraceID returns a random 32-hex-char W3C trace ID, generated once per
// logical operation (e.g. once per Upload call, reused across its retries)
// so every attempt correlates to the same trace.
func NewTraceID() (string, error) {
	return randomHex(16)
}

// TraceParent builds a W3C traceparent header value for traceID with a fresh
// random span ID: "00-<trace-id>-<span-id>-01". The sampled flag is always
// set — Core decides what to do with it.
func TraceParent(traceID string) (string, error) {
	spanID, err := randomHex(8)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("00-%s-%s-01", traceID, spanID), nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("tracing: rand read: %w", err)
	}
	return hex.EncodeToString(b), nil
}
