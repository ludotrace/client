package uploader

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ludotrace/client/internal/tracing"
)

var (
	ErrUnauthorized = errors.New("uploader: unauthorized (401)")
	ErrFileTooLarge = errors.New("uploader: file too large (413)")
	ErrLimitReached = errors.New("uploader: upload limit reached (429)")
	ErrTransient    = errors.New("uploader: transient failure after retries")
)

// LimitReachedError is returned on a 429 upload_limit_reached response. It
// wraps the ErrLimitReached sentinel (so existing errors.Is checks keep
// working) and carries an optional server-provided Retry-After hint.
//
// RetryAfter is the parsed value of the response's RFC 7231 Retry-After
// header, converted to a delay from the moment the response was received. A
// zero value means the server did not send a usable hint (header absent,
// non-positive, or unparseable) and the caller should fall back to its own
// default wait.
type LimitReachedError struct {
	RetryAfter time.Duration
}

func (e *LimitReachedError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("uploader: upload limit reached (429), retry after %s", e.RetryAfter)
	}
	return ErrLimitReached.Error()
}

func (e *LimitReachedError) Unwrap() error { return ErrLimitReached }

// parseRetryAfter parses an RFC 7231 Retry-After header value relative to now.
// It supports both defined forms: delta-seconds (a non-negative integer number
// of seconds) and HTTP-date. It returns (d, true) only when the value parses to
// a strictly positive delay; an empty, non-positive, past-dated, or unparseable
// value yields (0, false), signalling the caller to use its own fallback. Go's
// net/http parses HTTP-date on the client side via http.ParseTime, but does not
// parse the Retry-After header itself, so this handles both forms.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}

	// delta-seconds form: 1*DIGIT, no sign per the grammar. A "0" or malformed
	// negative carries no useful "wait this long" signal, so treat as absent.
	if secs, err := strconv.Atoi(value); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}

	// HTTP-date form.
	if t, err := http.ParseTime(value); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
		return 0, false
	}

	return 0, false
}

type ErrBadRequest struct {
	Reason string
}

func (e ErrBadRequest) Error() string {
	return fmt.Sprintf("uploader: bad request (400): %s", e.Reason)
}

var httpClient = &http.Client{Timeout: 120 * time.Second}

type uploadResponse struct {
	JobID string `json:"job_id"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// Upload POSTs the session at filePath to Core, retrying transient failures.
// It also returns the W3C trace ID generated for this call (client#64,
// companion to core#80) — a fresh ID per Upload call, reused across its
// retries so every attempt shows up on the same trace once Core's OTel
// pipeline honors the incoming traceparent. Callers surface it on a failed
// upload so a support case can be correlated against Core's trace.
func Upload(ctx context.Context, coreURL, gameID, filePath, token string) (string, string, error) {
	backoffs := []time.Duration{0, 200 * time.Millisecond, 400 * time.Millisecond}

	traceID, err := tracing.NewTraceID()
	if err != nil {
		// Best-effort: tracing must never block an upload. An empty traceID
		// just means doUpload skips the traceparent header for this call.
		traceID = ""
	}

	var lastErr error
	for attempt, delay := range backoffs {
		if attempt > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", traceID, ctx.Err()
			case <-timer.C:
			}
		}

		jobID, err := doUpload(ctx, coreURL, gameID, filePath, token, traceID)
		if err == nil {
			return jobID, traceID, nil
		}

		// Only retry on transient errors; permanent 4xx errors return immediately.
		if !isTransient(err) {
			return "", traceID, err
		}
		lastErr = err
	}

	return "", traceID, fmt.Errorf("%w: %v", ErrTransient, lastErr)
}

func isTransient(err error) bool {
	if err == nil {
		return false
	}
	// Sentinel errors from 4xx responses are permanent.
	if errors.Is(err, ErrUnauthorized) ||
		errors.Is(err, ErrFileTooLarge) ||
		errors.Is(err, ErrLimitReached) {
		return false
	}
	var badReq ErrBadRequest
	if errors.As(err, &badReq) {
		return false
	}
	return true
}

func doUpload(ctx context.Context, coreURL, gameID, filePath, token, traceID string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	pr, pw := io.Pipe()

	mw := multipart.NewWriter(pw)

	go func() {
		var writeErr error
		defer func() {
			// Close multipart writer first to write the final boundary, then close
			// the pipe so the reader sees EOF (or the error).
			if cerr := mw.Close(); cerr != nil && writeErr == nil {
				writeErr = cerr
			}
			pw.CloseWithError(writeErr)
		}()

		if writeErr = mw.WriteField("game_id", gameID); writeErr != nil {
			return
		}

		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="session_file"; filename="session.jsonl.gz"`)
		h.Set("Content-Type", "application/gzip")

		var part io.Writer
		part, writeErr = mw.CreatePart(h)
		if writeErr != nil {
			return
		}

		gz := gzip.NewWriter(part)
		if _, writeErr = io.Copy(gz, f); writeErr != nil {
			return
		}
		writeErr = gz.Close()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, coreURL+"/v1/upload", pr)
	if err != nil {
		// Drain the pipe writer goroutine so it doesn't leak.
		pr.CloseWithError(err)
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	if traceID != "" {
		if traceParent, err := tracing.TraceParent(traceID); err == nil {
			req.Header.Set("traceparent", traceParent)
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, 64*1024)

	switch resp.StatusCode {
	case http.StatusAccepted:
		var ur uploadResponse
		if err := json.NewDecoder(limited).Decode(&ur); err != nil {
			return "", fmt.Errorf("uploader: decode 202 body: %w", err)
		}
		return ur.JobID, nil

	case http.StatusUnauthorized:
		return "", ErrUnauthorized

	case http.StatusRequestEntityTooLarge:
		return "", ErrFileTooLarge

	case http.StatusTooManyRequests:
		lre := &LimitReachedError{}
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
			lre.RetryAfter = d
		}
		return "", lre

	case http.StatusBadRequest:
		var er errorResponse
		if err := json.NewDecoder(limited).Decode(&er); err != nil {
			return "", ErrBadRequest{Reason: "unknown"}
		}
		return "", ErrBadRequest{Reason: er.Error}

	default:
		// 5xx and anything else we haven't handled explicitly.
		body, _ := io.ReadAll(limited)
		return "", fmt.Errorf("uploader: unexpected status %d: %s", resp.StatusCode, body)
	}
}
