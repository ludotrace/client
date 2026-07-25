package uploader

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "session*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestUpload_202(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, jsonBody(map[string]string{"job_id": "abc123"}))
	}))
	defer srv.Close()

	filePath := writeTempFile(t, `{"type":"session_start"}`)
	jobID, _, err := Upload(context.Background(), srv.URL, "fallout4", filePath, "tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jobID != "abc123" {
		t.Fatalf("got jobID %q, want %q", jobID, "abc123")
	}
}

func TestUpload_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	filePath := writeTempFile(t, "data")
	_, _, err := Upload(context.Background(), srv.URL, "fallout4", filePath, "bad")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("got %v, want ErrUnauthorized", err)
	}
}

func TestUpload_413(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))
	defer srv.Close()

	filePath := writeTempFile(t, "data")
	_, _, err := Upload(context.Background(), srv.URL, "fallout4", filePath, "tok")
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("got %v, want ErrFileTooLarge", err)
	}
}

func TestUpload_429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	filePath := writeTempFile(t, "data")
	_, _, err := Upload(context.Background(), srv.URL, "fallout4", filePath, "tok")
	if !errors.Is(err, ErrLimitReached) {
		t.Fatalf("got %v, want ErrLimitReached", err)
	}
}

func TestUpload_429_RetryAfterDeltaSeconds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "43200")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	filePath := writeTempFile(t, "data")
	_, _, err := Upload(context.Background(), srv.URL, "fallout4", filePath, "tok")

	if !errors.Is(err, ErrLimitReached) {
		t.Fatalf("got %v, want error wrapping ErrLimitReached", err)
	}
	var lre *LimitReachedError
	if !errors.As(err, &lre) {
		t.Fatalf("got %v, want *LimitReachedError", err)
	}
	if lre.RetryAfter != 43200*time.Second {
		t.Fatalf("got RetryAfter %v, want %v", lre.RetryAfter, 43200*time.Second)
	}
}

func TestUpload_429_RetryAfterHTTPDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", time.Now().Add(2*time.Hour).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	filePath := writeTempFile(t, "data")
	_, _, err := Upload(context.Background(), srv.URL, "fallout4", filePath, "tok")

	var lre *LimitReachedError
	if !errors.As(err, &lre) {
		t.Fatalf("got %v, want *LimitReachedError", err)
	}
	// Allow slack for round-trip latency; HTTP-date has 1s resolution.
	if lre.RetryAfter < 110*time.Minute || lre.RetryAfter > 2*time.Hour {
		t.Fatalf("got RetryAfter %v, want ~2h", lre.RetryAfter)
	}
}

func TestUpload_429_NoRetryAfterHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	filePath := writeTempFile(t, "data")
	_, _, err := Upload(context.Background(), srv.URL, "fallout4", filePath, "tok")

	var lre *LimitReachedError
	if !errors.As(err, &lre) {
		t.Fatalf("got %v, want *LimitReachedError", err)
	}
	if lre.RetryAfter != 0 {
		t.Fatalf("got RetryAfter %v, want 0 (not provided)", lre.RetryAfter)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		value   string
		wantDur time.Duration
		wantOK  bool
	}{
		{"delta seconds", "43200", 43200 * time.Second, true},
		{"delta seconds one", "1", 1 * time.Second, true},
		{"delta seconds surrounding whitespace", "  120  ", 120 * time.Second, true},
		{"empty", "", 0, false},
		{"zero", "0", 0, false},
		{"negative", "-5", 0, false},
		{"non-numeric garbage", "soon", 0, false},
		{"http-date future", now.Add(90 * time.Minute).Format(http.TimeFormat), 90 * time.Minute, true},
		{"http-date past", now.Add(-90 * time.Minute).Format(http.TimeFormat), 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDur, gotOK := parseRetryAfter(tt.value, now)
			if gotOK != tt.wantOK {
				t.Fatalf("ok = %v, want %v", gotOK, tt.wantOK)
			}
			if gotOK && gotDur != tt.wantDur {
				t.Fatalf("dur = %v, want %v", gotDur, tt.wantDur)
			}
		})
	}
}

func TestUpload_400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, jsonBody(map[string]string{"error": "unknown_game"}))
	}))
	defer srv.Close()

	filePath := writeTempFile(t, "data")
	_, _, err := Upload(context.Background(), srv.URL, "fallout4", filePath, "tok")

	var badReq ErrBadRequest
	if !errors.As(err, &badReq) {
		t.Fatalf("got %v, want ErrBadRequest", err)
	}
	if badReq.Reason != "unknown_game" {
		t.Fatalf("got Reason %q, want %q", badReq.Reason, "unknown_game")
	}
}

func TestUpload_500_RetriesAndWrapsErrTransient(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	defer srv.Close()

	filePath := writeTempFile(t, "data")
	start := time.Now()
	_, _, err := Upload(context.Background(), srv.URL, "fallout4", filePath, "tok")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTransient) {
		t.Fatalf("got %v, want error wrapping ErrTransient", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("got %d calls, want 3", calls.Load())
	}
	// Total backoff is at least 200ms+400ms=600ms.
	if elapsed < 500*time.Millisecond {
		t.Fatalf("expected at least 500ms elapsed for backoffs, got %v", elapsed)
	}
}

func TestUpload_NetworkError_RetriesAndWrapsErrTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	// Close server immediately so all connection attempts fail.
	srv.Close()

	filePath := writeTempFile(t, "data")
	_, _, err := Upload(context.Background(), srv.URL, "fallout4", filePath, "tok")

	if !errors.Is(err, ErrTransient) {
		t.Fatalf("got %v, want error wrapping ErrTransient", err)
	}
}

func TestUpload_ContextCancelDuringBackoff(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	filePath := writeTempFile(t, "data")

	// Cancel after first attempt completes so the second backoff wait is interrupted.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, _, err := Upload(ctx, srv.URL, "fallout4", filePath, "tok")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestUpload_MultipartBodyValid(t *testing.T) {
	const fileContent = `{"type":"session_start","game":"fallout4"}` + "\n" +
		`{"type":"session_end"}`

	var receivedGameID string
	var receivedFileContent string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			http.Error(w, "bad content-type", http.StatusBadRequest)
			return
		}

		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			switch part.FormName() {
			case "game_id":
				b, _ := io.ReadAll(part)
				receivedGameID = string(b)
			case "session_file":
				gz, gerr := gzip.NewReader(part)
				if gerr != nil {
					http.Error(w, "not gzip: "+gerr.Error(), http.StatusBadRequest)
					return
				}
				b, _ := io.ReadAll(gz)
				gz.Close()
				receivedFileContent = string(b)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, jsonBody(map[string]string{"job_id": "xyz"}))
	}))
	defer srv.Close()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(filePath, []byte(fileContent), 0o644); err != nil {
		t.Fatal(err)
	}

	jobID, _, err := Upload(context.Background(), srv.URL, "fallout4", filePath, "tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jobID != "xyz" {
		t.Fatalf("got jobID %q, want %q", jobID, "xyz")
	}
	if receivedGameID != "fallout4" {
		t.Fatalf("got game_id %q, want %q", receivedGameID, "fallout4")
	}
	if receivedFileContent != fileContent {
		t.Fatalf("round-trip mismatch:\ngot:  %q\nwant: %q", receivedFileContent, fileContent)
	}
}
