package uploader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ludotrace/client/internal/capture"
)

// captureFieldServer accepts an upload and records the capture_context form
// field exactly as Core would read it.
func captureFieldServer(t *testing.T, got *string, present *bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
		}
		_, *present = r.MultipartForm.Value["capture_context"]
		*got = r.FormValue("capture_context")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, jsonBody(map[string]string{"job_id": "abc123"}))
	}))
}

// TestUpload_SendsCaptureContext checks the field Core validates. The enum
// values are a contract with core/internal/capture: a value it does not
// recognize rejects the whole upload, so the wire form is worth pinning.
func TestUpload_SendsCaptureContext(t *testing.T) {
	var body string
	var present bool
	srv := captureFieldServer(t, &body, &present)
	defer srv.Close()

	gap := 300
	cc := &capture.Context{
		Opener:     capture.OpenerAbsent,
		ClosedBy:   capture.ClosedByIdleTimeout,
		GapBefore:  &gap,
		EventCount: 605,
		SpanS:      2330,
	}

	filePath := writeTempFile(t, `{"type":"kill"}`)
	if _, _, err := Upload(context.Background(), srv.URL, "fallout4", filePath, "tok", cc); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !present {
		t.Fatal("capture_context field absent")
	}

	var back map[string]any
	if err := json.Unmarshal([]byte(body), &back); err != nil {
		t.Fatalf("capture_context is not JSON: %q", body)
	}
	for k, want := range map[string]any{
		"opener":      "absent",
		"closed_by":   "idle_timeout",
		"gap_before":  float64(300),
		"event_count": float64(605),
		"span_s":      float64(2330),
	} {
		if back[k] != want {
			t.Errorf("%s = %v, want %v", k, back[k], want)
		}
	}
	// Core rejects unknown fields outright, so the object must carry nothing else.
	if len(back) != 5 {
		t.Errorf("capture_context has %d fields, want 5: %q", len(back), body)
	}
}

// TestUpload_OmitsCaptureContextWhenNil: a queue item enqueued before capture
// context existed has none, and Core must see no field at all rather than an
// empty or partial object — a partial one is rejected, and the upload with it.
func TestUpload_OmitsCaptureContextWhenNil(t *testing.T) {
	var body string
	var present bool
	srv := captureFieldServer(t, &body, &present)
	defer srv.Close()

	filePath := writeTempFile(t, `{"type":"kill"}`)
	if _, _, err := Upload(context.Background(), srv.URL, "fallout4", filePath, "tok", nil); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if present {
		t.Errorf("capture_context sent despite none being reported: %q", body)
	}
}

// TestUpload_OmitsGapBeforeWhenUnmeasurable is the pairing that matters most on
// the wire: gap_before must vanish, not serialize as 0. Zero is a reported gap
// the model may read as a real boundary; absent means it was never measured.
func TestUpload_OmitsGapBeforeWhenUnmeasurable(t *testing.T) {
	var body string
	var present bool
	srv := captureFieldServer(t, &body, &present)
	defer srv.Close()

	cc := &capture.Context{
		Opener:     capture.OpenerPresent,
		ClosedBy:   capture.ClosedByClientShutdown,
		EventCount: 10,
		SpanS:      60,
	}

	filePath := writeTempFile(t, `{"type":"kill"}`)
	if _, _, err := Upload(context.Background(), srv.URL, "fallout4", filePath, "tok", cc); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	var back map[string]any
	if err := json.Unmarshal([]byte(body), &back); err != nil {
		t.Fatalf("capture_context is not JSON: %q", body)
	}
	if _, ok := back["gap_before"]; ok {
		t.Errorf("gap_before serialized despite not being measured: %q", body)
	}
}
