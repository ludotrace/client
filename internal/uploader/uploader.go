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
	"time"
)

var (
	ErrUnauthorized = errors.New("uploader: unauthorized (401)")
	ErrFileTooLarge = errors.New("uploader: file too large (413)")
	ErrLimitReached = errors.New("uploader: upload limit reached (429)")
	ErrTransient    = errors.New("uploader: transient failure after retries")
)

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

func Upload(ctx context.Context, coreURL, gameID, filePath, token string) (string, error) {
	backoffs := []time.Duration{0, 200 * time.Millisecond, 400 * time.Millisecond}

	var lastErr error
	for attempt, delay := range backoffs {
		if attempt > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
		}

		jobID, err := doUpload(ctx, coreURL, gameID, filePath, token)
		if err == nil {
			return jobID, nil
		}

		// Only retry on transient errors; permanent 4xx errors return immediately.
		if !isTransient(err) {
			return "", err
		}
		lastErr = err
	}

	return "", fmt.Errorf("%w: %v", ErrTransient, lastErr)
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

func doUpload(ctx context.Context, coreURL, gameID, filePath, token string) (string, error) {
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
		return "", ErrLimitReached

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
