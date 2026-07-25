package tracing

import (
	"regexp"
	"testing"
)

var traceParentRE = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)

func TestNewTraceID(t *testing.T) {
	id, err := NewTraceID()
	if err != nil {
		t.Fatalf("NewTraceID: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("NewTraceID = %q, want 32 lowercase hex chars", id)
	}

	id2, err := NewTraceID()
	if err != nil {
		t.Fatalf("NewTraceID: %v", err)
	}
	if id == id2 {
		t.Fatal("NewTraceID returned the same ID twice")
	}
}

func TestTraceParent(t *testing.T) {
	traceID, err := NewTraceID()
	if err != nil {
		t.Fatalf("NewTraceID: %v", err)
	}

	tp, err := TraceParent(traceID)
	if err != nil {
		t.Fatalf("TraceParent: %v", err)
	}
	if !traceParentRE.MatchString(tp) {
		t.Fatalf("TraceParent = %q, want format 00-<32hex>-<16hex>-01", tp)
	}

	tp2, err := TraceParent(traceID)
	if err != nil {
		t.Fatalf("TraceParent: %v", err)
	}
	if tp == tp2 {
		t.Fatal("TraceParent returned the same span ID twice for the same trace")
	}
	if tp[3:35] != traceID {
		t.Fatalf("TraceParent embedded trace ID = %q, want %q", tp[3:35], traceID)
	}
}
