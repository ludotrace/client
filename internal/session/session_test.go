package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ludotrace/client/internal/queue"
)

func newTestQueue(t *testing.T) *queue.Queue {
	t.Helper()
	dir := t.TempDir()
	q, err := queue.New(filepath.Join(dir, "queue.json"))
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}
	return q
}

func writeEvents(t *testing.T, path string, lines []string) {
	t.Helper()
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("writeEvents: %v", err)
	}
}

// makeOld backdates the events file past the inactivity threshold so an open
// session is flushed as finished. session_end no longer triggers a flush on its
// own, so a test that wants a terminal session enqueued must mark the file idle.
func makeOld(t *testing.T, path string) {
	t.Helper()
	past := time.Now().Add(-(orphanThreshold + time.Minute))
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
}

// TestOrphanThresholdValue pins the inactivity boundary. The docs (CLAUDE.md
// Watcher Logic, README, PRD) all quote this number, so a change here must be a
// deliberate one that updates them too.
func TestOrphanThresholdValue(t *testing.T) {
	if want := 12 * time.Minute; orphanThreshold != want {
		t.Fatalf("orphanThreshold = %v, want %v", orphanThreshold, want)
	}
}

func TestExtract_OneCompleteSession(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	lines := []string{
		`{"type":"session_start","session_id":"s1"}`,
		`{"type":"kill","target":"radroach"}`,
		`{"type":"session_end","session_id":"s1"}`,
	}
	writeEvents(t, eventsPath, lines)
	makeOld(t, eventsPath) // session flushes on inactivity, not on session_end

	e := New("fallout4", eventsPath, offsetPath, dir, q)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if q.Len() != 1 {
		t.Fatalf("expected 1 queued item, got %d", q.Len())
	}
	item, ok := q.Peek()
	if !ok {
		t.Fatal("Peek returned false")
	}
	if item.GameID != "fallout4" {
		t.Errorf("GameID = %q, want %q", item.GameID, "fallout4")
	}
	if item.TmpPath == "" {
		t.Error("TmpPath is empty")
	}

	data, err := os.ReadFile(item.TmpPath)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(got) != 3 {
		t.Errorf("temp file has %d lines, want 3", len(got))
	}
}

// A replaced events file — mod reinstalled, file deleted and recreated, or
// renamed — is shorter than the stored offset. Seeking past EOF reads nothing
// and never advances the offset, so without a size check the game would stop
// uploading permanently. Extract must restart from zero instead.
func TestExtract_FileShorterThanOffset_RestartsFromZero(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	// An offset far beyond the end of the fresh file, as if left behind by a
	// much longer predecessor.
	if err := os.WriteFile(offsetPath, []byte("999999\n"), 0600); err != nil {
		t.Fatalf("write offset: %v", err)
	}

	lines := []string{
		`{"type":"session_start","session_id":"s1"}`,
		`{"type":"session_end","session_id":"s1"}`,
	}
	writeEvents(t, eventsPath, lines)
	makeOld(t, eventsPath)

	e := New("fallout4", eventsPath, offsetPath, dir, q)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if q.Len() != 1 {
		t.Fatalf("expected the new file's session to be queued, got %d items", q.Len())
	}
}

// TestExtract_InactivityBoundary walks the threshold from both sides: a pause
// shorter than orphanThreshold leaves the session open, a longer one flushes it.
func TestExtract_InactivityBoundary(t *testing.T) {
	cases := []struct {
		name    string
		idle    time.Duration
		wantLen int
	}{
		{"just under threshold stays open", orphanThreshold - time.Minute, 0},
		{"just over threshold flushes", orphanThreshold + time.Minute, 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			eventsPath := filepath.Join(dir, "events.jsonl")
			offsetPath := filepath.Join(dir, "offset")
			q := newTestQueue(t)

			writeEvents(t, eventsPath, []string{
				`{"type":"session_start","session_id":"s1"}`,
				`{"type":"kill","target":"radroach"}`,
			})
			past := time.Now().Add(-tc.idle)
			if err := os.Chtimes(eventsPath, past, past); err != nil {
				t.Fatalf("Chtimes: %v", err)
			}

			e := New("fallout4", eventsPath, offsetPath, dir, q)
			if err := e.Extract(); err != nil {
				t.Fatalf("Extract: %v", err)
			}
			if q.Len() != tc.wantLen {
				t.Errorf("after %v idle: queued %d, want %d", tc.idle, q.Len(), tc.wantLen)
			}
		})
	}
}

func TestExtract_TwoConsecutiveSessions(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	lines := []string{
		`{"type":"session_start","session_id":"s1"}`,
		`{"type":"kill","target":"radroach"}`,
		`{"type":"session_end","session_id":"s1"}`,
		`{"type":"session_start","session_id":"s2"}`,
		`{"type":"quest_stage","quest":"main001","stage":10}`,
		`{"type":"session_end","session_id":"s2"}`,
	}
	writeEvents(t, eventsPath, lines)
	// s1 flushes when s2's session_start is seen; s2 flushes on inactivity.
	makeOld(t, eventsPath)

	e := New("fallout4", eventsPath, offsetPath, dir, q)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if q.Len() != 2 {
		t.Fatalf("expected 2 queued items, got %d", q.Len())
	}
}

// TestExtract_SessionEndIsNotABoundary covers the core fix: events that arrive
// after a session_end belong to the same play session and must not be lost.
// Fallout 4 writes a session_end on every save, so play continuing after a save
// is the common case, not an edge case.
func TestExtract_SessionEndContinuesSession(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	lines := []string{
		`{"type":"session_start","session_id":"s1"}`,
		`{"type":"kill","target":"radroach"}`,
		`{"type":"session_end","session_id":"s1"}`, // a save mid-session
		`{"type":"kill","target":"deathclaw"}`,     // played on after the save
		`{"type":"session_end","session_id":"s1"}`, // another save
		`{"type":"kill","target":"mirelurk"}`,      // and quit/crashed with no final save
	}
	writeEvents(t, eventsPath, lines)

	e := New("fallout4", eventsPath, offsetPath, dir, q)

	// Recent file: the session is still open and nothing is enqueued, even
	// though two session_end lines have been seen.
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract (recent): %v", err)
	}
	if q.Len() != 0 {
		t.Fatalf("recent file with session_end should not enqueue, got %d", q.Len())
	}

	// Once the file goes idle the whole session flushes as one bundle —
	// including every event after each session_end.
	makeOld(t, eventsPath)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract (idle): %v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("expected 1 queued item, got %d", q.Len())
	}
	item, _ := q.Peek()
	data, err := os.ReadFile(item.TmpPath)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	for _, want := range []string{"radroach", "deathclaw", "mirelurk"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("bundle missing %q (post-session_end data lost): %s", want, data)
		}
	}
}

// TestExtract_NewSessionFlushesUnclosedPrior covers the second data-loss path:
// a session_start with no preceding session_end (e.g. a reload after a save-less
// crash) must flush the prior session rather than discard it.
func TestExtract_NewSessionFlushesUnclosedPrior(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	lines := []string{
		`{"type":"session_start","session_id":"s1"}`,
		`{"type":"kill","target":"raider"}`,
		// no session_end for s1 — game crashed, then was relaunched:
		`{"type":"session_start","session_id":"s2"}`,
		`{"type":"kill","target":"ghoul"}`,
		`{"type":"session_end","session_id":"s2"}`,
	}
	writeEvents(t, eventsPath, lines)
	makeOld(t, eventsPath) // flush the still-open s2 as well

	e := New("fallout4", eventsPath, offsetPath, dir, q)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if q.Len() != 2 {
		t.Fatalf("expected 2 queued items (s1 flushed by s2 start, s2 by inactivity), got %d", q.Len())
	}
	first, _ := q.Peek()
	data, err := os.ReadFile(first.TmpPath)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	if !strings.Contains(string(data), "raider") || strings.Contains(string(data), "ghoul") {
		t.Errorf("first bundle should be s1 only (the unclosed prior), got: %s", data)
	}
}

func TestExtract_OpenSessionRecentFile_NoEnqueue(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	lines := []string{
		`{"type":"session_start","session_id":"s1"}`,
		`{"type":"kill","target":"radroach"}`,
	}
	writeEvents(t, eventsPath, lines)
	// File mod time is recent (just written), so no orphan.

	e := New("fallout4", eventsPath, offsetPath, dir, q)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if q.Len() != 0 {
		t.Errorf("expected 0 queued items, got %d", q.Len())
	}
}

func TestExtract_OrphanSession_OldModTime(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	lines := []string{
		`{"type":"session_start","session_id":"s1"}`,
		`{"type":"kill","target":"deathclaw"}`,
	}
	writeEvents(t, eventsPath, lines)

	past := time.Now().Add(-31 * time.Minute)
	if err := os.Chtimes(eventsPath, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	e := New("fallout4", eventsPath, offsetPath, dir, q)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if q.Len() != 1 {
		t.Errorf("expected 1 orphan queued, got %d", q.Len())
	}
	item, _ := q.Peek()
	if item.GameID != "fallout4" {
		t.Errorf("orphan GameID = %q, want %q", item.GameID, "fallout4")
	}
}

func TestExtract_ReadsFromPersistedOffset(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	session1Lines := []string{
		`{"type":"session_start","session_id":"s1"}`,
		`{"type":"session_end","session_id":"s1"}`,
	}
	session2Lines := []string{
		`{"type":"session_start","session_id":"s2"}`,
		`{"type":"kill","target":"super_mutant"}`,
		`{"type":"session_end","session_id":"s2"}`,
	}

	allLines := append(session1Lines, session2Lines...)
	writeEvents(t, eventsPath, allLines)

	// Calculate byte offset past session 1.
	var offsetPastS1 int64
	for _, l := range session1Lines {
		offsetPastS1 += int64(len(l)) + 1 // +1 for newline
	}

	if err := writeOffsetFile(offsetPath, offsetPastS1); err != nil {
		t.Fatalf("writeOffsetFile: %v", err)
	}
	makeOld(t, eventsPath) // flush s2 on inactivity

	e := New("fallout4", eventsPath, offsetPath, dir, q)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if q.Len() != 1 {
		t.Fatalf("expected 1 item (s2 only), got %d", q.Len())
	}
	item, _ := q.Peek()

	data, err := os.ReadFile(item.TmpPath)
	if err != nil {
		t.Fatalf("read temp: %v", err)
	}
	if !strings.Contains(string(data), "s2") {
		t.Errorf("temp file should contain s2 content, got: %s", data)
	}
}

func TestAdvanceOffset_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	e := New("fallout4", eventsPath, offsetPath, dir, q)

	const wantOffset int64 = 12345
	if err := e.AdvanceOffset(wantOffset); err != nil {
		t.Fatalf("AdvanceOffset: %v", err)
	}

	got, err := e.readOffset()
	if err != nil {
		t.Fatalf("readOffset: %v", err)
	}
	if got != wantOffset {
		t.Errorf("offset = %d, want %d", got, wantOffset)
	}
}

// The upload worker retires items newest-first, so it can hand back a lower
// end offset after a higher one. The high-water mark must hold, or Extract()
// rewinds and rebuilds sessions the pipeline has already finished with.
func TestAdvanceOffset_NeverMovesBackwards(t *testing.T) {
	dir := t.TempDir()
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	e := New("fallout4", filepath.Join(dir, "events.jsonl"), offsetPath, dir, q)

	if err := e.AdvanceOffset(900); err != nil {
		t.Fatalf("AdvanceOffset(900): %v", err)
	}
	for _, older := range []int64{400, 900, 0} {
		if err := e.AdvanceOffset(older); err != nil {
			t.Fatalf("AdvanceOffset(%d): %v", older, err)
		}
		got, err := e.readOffset()
		if err != nil {
			t.Fatalf("readOffset: %v", err)
		}
		if got != 900 {
			t.Fatalf("after AdvanceOffset(%d): offset = %d, want 900", older, got)
		}
	}

	if err := e.AdvanceOffset(1200); err != nil {
		t.Fatalf("AdvanceOffset(1200): %v", err)
	}
	got, err := e.readOffset()
	if err != nil {
		t.Fatalf("readOffset: %v", err)
	}
	if got != 1200 {
		t.Errorf("offset = %d, want 1200 — a forward advance must still apply", got)
	}
}

func TestExtract_MissingEventsFile_NoError(t *testing.T) {
	dir := t.TempDir()
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	e := New("fallout4", filepath.Join(dir, "nonexistent.jsonl"), offsetPath, dir, q)
	if err := e.Extract(); err != nil {
		t.Errorf("Extract on missing file should return nil, got: %v", err)
	}
}

func TestExtract_Concurrent_NoPanic(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	var sb strings.Builder
	for i := 0; i < 10; i++ {
		sb.WriteString(fmt.Sprintf("{\"type\":\"session_start\",\"session_id\":\"s%d\"}\n", i))
		sb.WriteString(fmt.Sprintf("{\"type\":\"session_end\",\"session_id\":\"s%d\"}\n", i))
	}
	if err := os.WriteFile(eventsPath, []byte(sb.String()), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	e := New("fallout4", eventsPath, offsetPath, dir, q)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.Extract()
		}()
	}
	wg.Wait()
}
