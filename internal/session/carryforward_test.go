package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ludotrace/client/internal/capture"
	"github.com/ludotrace/client/internal/queue"
)

// readOffsetFile reads the sidecar directly, so a test can assert the mark did
// not move — the whole point of holding rather than uploading.
func readOffsetFile(t *testing.T, path string) int64 {
	t.Helper()
	e := &Extractor{offsetPath: path}
	off, err := e.readOffset()
	if err != nil {
		t.Fatalf("readOffset: %v", err)
	}
	return off
}

func queuedLines(t *testing.T, item queue.Item) []string {
	t.Helper()
	data, err := os.ReadFile(item.TmpPath)
	if err != nil {
		t.Fatalf("read temp file: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// TestExtract_CarriesForwardOrphanedBytes is ludotrace/client#75 itself.
//
// The inactivity flush uploads a session and advances the offset to where the
// file ended at that moment. The player then resumes, so events append past
// that offset with their session_start already behind it. Every one of those
// events used to be discarded — permanently, because the offset only advances
// on a successful upload, so the same bytes were re-read and re-dropped forever.
//
// They must instead be held and prepended to the next session as its lead-in.
func TestExtract_CarriesForwardOrphanedBytes(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	// Everything up to and including the flushed session's last event.
	flushed := []string{
		`{"type":"session_start","wall_time":"100"}`,
		`{"type":"kill","wall_time":"200"}`,
	}
	// The orphans: real play, no opener, because the opener was in the region
	// already uploaded above.
	orphaned := []string{
		`{"type":"quest_stage","wall_time":"900"}`,
		`{"type":"kill","wall_time":"1000"}`,
	}
	// The save load that gives the held bytes a session to lead into.
	resumed := []string{
		`{"type":"session_start","wall_time":"1300"}`,
		`{"type":"kill","wall_time":"1400"}`,
	}

	writeEvents(t, eventsPath, append(append(append([]string{}, flushed...), orphaned...), resumed...))

	// Put the offset exactly where the inactivity flush would have left it:
	// past the flushed session, before the orphans.
	flushedBytes := int64(len(strings.Join(flushed, "\n")) + 1)
	if err := writeOffsetFile(offsetPath, flushedBytes); err != nil {
		t.Fatalf("writeOffsetFile: %v", err)
	}

	makeOld(t, eventsPath)

	e := New("fallout4", eventsPath, offsetPath, dir, q)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if q.Len() != 1 {
		t.Fatalf("expected 1 queued item, got %d", q.Len())
	}
	item, _ := q.Peek()

	got := queuedLines(t, item)
	if len(got) != 4 {
		t.Fatalf("uploaded %d events, want 4 (2 carried forward + 2 resumed):\n%s",
			len(got), strings.Join(got, "\n"))
	}
	// Held bytes lead in, in file order, ahead of the session that released them.
	if !strings.Contains(got[0], `"quest_stage"`) || !strings.Contains(got[2], `"session_start"`) {
		t.Errorf("carried-forward bytes are not leading the upload:\n%s", strings.Join(got, "\n"))
	}

	cc := item.Capture
	if cc == nil {
		t.Fatal("no capture context on a carried-forward upload")
	}
	if cc.Opener != capture.OpenerAbsent {
		t.Errorf("opener = %q, want %q — the run's session_start was uploaded earlier",
			cc.Opener, capture.OpenerAbsent)
	}
	if cc.ClosedBy != capture.ClosedByIdleTimeout {
		t.Errorf("closed_by = %q, want %q", cc.ClosedBy, capture.ClosedByIdleTimeout)
	}
	if cc.GapBefore == nil {
		t.Fatal("gap_before missing; the wall_time jump across the lead-in reads as continuous play")
	}
	if *cc.GapBefore != 300 { // 1300 - 1000
		t.Errorf("gap_before = %d, want 300", *cc.GapBefore)
	}
	if cc.EventCount != 4 {
		t.Errorf("event_count = %d, want 4", cc.EventCount)
	}
}

// TestExtract_HoldsOrphanedBytesUntilTheyGainAnOpener is the other half: a hold
// under the cap is never uploaded on its own, however long it sits. A fragment
// with no opener is not a candidate for its own insight, so uploading it would
// spend an inference run on something the model cannot place.
//
// The offset must not move either — that is what keeps holding lossless.
func TestExtract_HoldsOrphanedBytesUntilTheyGainAnOpener(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	// No session_start anywhere in the read region.
	writeEvents(t, eventsPath, []string{
		`{"type":"quest_stage","wall_time":"900"}`,
		`{"type":"kill","wall_time":"1000"}`,
	})
	makeOld(t, eventsPath) // idle well past the threshold

	e := New("fallout4", eventsPath, offsetPath, dir, q)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if q.Len() != 0 {
		t.Errorf("openerless fragment was uploaded alone; queued %d items", q.Len())
	}
	if off := readOffsetFile(t, offsetPath); off != 0 {
		t.Errorf("offset advanced to %d past held bytes; they would be lost", off)
	}

	// Repeated passes must be idempotent — the hold is rebuilt from disk each
	// time, not accumulated.
	if err := e.Extract(); err != nil {
		t.Fatalf("second Extract: %v", err)
	}
	if q.Len() != 0 {
		t.Errorf("second pass queued %d items", q.Len())
	}
}

// TestExtract_ReleasesHoldAtSizeCap: holding cannot grow without bound. A hold
// past the cap is released on its own, reporting size_cap — it was cut
// mid-play, so it must not claim the player stopped or the client exited.
func TestExtract_ReleasesHoldAtSizeCap(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	// Openerless, and big enough to trip the cap.
	filler := strings.Repeat("x", 512)
	var lines []string
	for i := 0; i < (holdCap/512)+16; i++ {
		lines = append(lines, `{"type":"kill","target":"`+filler+`"}`)
	}
	writeEvents(t, eventsPath, lines)

	e := New("fallout4", eventsPath, offsetPath, dir, q)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if q.Len() != 1 {
		t.Fatalf("expected the oversized hold to be released, got %d queued", q.Len())
	}
	item, _ := q.Peek()
	if item.Capture == nil {
		t.Fatal("no capture context on a cap-released hold")
	}
	if item.Capture.ClosedBy != capture.ClosedBySizeCap {
		t.Errorf("closed_by = %q, want %q", item.Capture.ClosedBy, capture.ClosedBySizeCap)
	}
	if item.Capture.Opener != capture.OpenerAbsent {
		t.Errorf("opener = %q, want %q", item.Capture.Opener, capture.OpenerAbsent)
	}
}

// TestExtract_CompleteSessionReportsPresentOpener pins the ordinary case, so
// the capture context cannot start claiming every upload is partial.
func TestExtract_CompleteSessionReportsPresentOpener(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	writeEvents(t, eventsPath, []string{
		`{"type":"session_start","wall_time":"2024-11-15T14:00:00Z"}`,
		`{"type":"kill","wall_time":"2024-11-15T14:20:00Z"}`,
	})
	makeOld(t, eventsPath)

	e := New("stardew", eventsPath, offsetPath, dir, q)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	item, ok := q.Peek()
	if !ok {
		t.Fatal("nothing queued")
	}
	cc := item.Capture
	if cc == nil {
		t.Fatal("no capture context")
	}
	if cc.Opener != capture.OpenerPresent {
		t.Errorf("opener = %q, want %q", cc.Opener, capture.OpenerPresent)
	}
	if cc.ClosedBy != capture.ClosedByIdleTimeout {
		t.Errorf("closed_by = %q, want %q", cc.ClosedBy, capture.ClosedByIdleTimeout)
	}
	if cc.GapBefore != nil {
		t.Errorf("gap_before = %d on a session with no lead-in; want omitted", *cc.GapBefore)
	}
	if cc.SpanS != 1200 {
		t.Errorf("span_s = %d, want 1200", cc.SpanS)
	}
}

// TestExtract_SupersededSessionReportsSuperseded: a session closed by the next
// session_start is a different closer from the inactivity flush, and the model
// is told which.
func TestExtract_SupersededSessionReportsSuperseded(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	writeEvents(t, eventsPath, []string{
		`{"type":"session_start","wall_time":"100"}`,
		`{"type":"kill","wall_time":"200"}`,
		`{"type":"session_start","wall_time":"300"}`,
		`{"type":"kill","wall_time":"400"}`,
	})
	makeOld(t, eventsPath)

	e := New("fallout4", eventsPath, offsetPath, dir, q)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if q.Len() != 2 {
		t.Fatalf("expected 2 sessions, got %d", q.Len())
	}
	first, _ := q.Peek()
	if first.Capture.ClosedBy != capture.ClosedBySuperseded {
		t.Errorf("first closed_by = %q, want %q", first.Capture.ClosedBy, capture.ClosedBySuperseded)
	}
	if first.Capture.Opener != capture.OpenerPresent {
		t.Errorf("first opener = %q, want %q", first.Capture.Opener, capture.OpenerPresent)
	}
}

// TestFlushForShutdown closes an open session on the Client's own exit rather
// than leaving it for an indefinite next launch. The events file is fresh here,
// so the inactivity flush explicitly does not fire — only shutdown does.
func TestFlushForShutdown(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	writeEvents(t, eventsPath, []string{
		`{"type":"session_start","wall_time":"100"}`,
		`{"type":"kill","wall_time":"200"}`,
	})

	e := New("fallout4", eventsPath, offsetPath, dir, q)

	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if q.Len() != 0 {
		t.Fatalf("a recently-written open session was flushed without a boundary; queued %d", q.Len())
	}

	if err := e.FlushForShutdown(); err != nil {
		t.Fatalf("FlushForShutdown: %v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("shutdown did not flush the open session; queued %d", q.Len())
	}
	item, _ := q.Peek()
	if item.Capture.ClosedBy != capture.ClosedByClientShutdown {
		t.Errorf("closed_by = %q, want %q", item.Capture.ClosedBy, capture.ClosedByClientShutdown)
	}
}

// TestFlushForShutdown_DoesNotReleaseHold: exiting says nothing about whether
// held bytes will gain an opener, so shutdown must not turn a hold into a
// standalone openerless upload.
func TestFlushForShutdown_DoesNotReleaseHold(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	writeEvents(t, eventsPath, []string{
		`{"type":"quest_stage","wall_time":"900"}`,
		`{"type":"kill","wall_time":"1000"}`,
	})

	e := New("fallout4", eventsPath, offsetPath, dir, q)
	if err := e.FlushForShutdown(); err != nil {
		t.Fatalf("FlushForShutdown: %v", err)
	}
	if q.Len() != 0 {
		t.Errorf("shutdown released a held fragment; queued %d", q.Len())
	}
	if off := readOffsetFile(t, offsetPath); off != 0 {
		t.Errorf("offset advanced to %d past held bytes", off)
	}
}

// TestExtract_GapOmittedAcrossGameRestart: Fallout 4's wall_time counts seconds
// since the game launched, so a relaunch restarts it from zero. The gap across
// that boundary is genuinely not in the data — real elapsed time between quit
// and relaunch was never recorded — so it must be omitted rather than reported
// as zero, which would assert continuity that never happened.
func TestExtract_GapOmittedAcrossGameRestart(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	writeEvents(t, eventsPath, []string{
		`{"type":"kill","wall_time":"5000"}`,       // held, late in the old run
		`{"type":"session_start","wall_time":"3"}`, // counter reset by relaunch
		`{"type":"kill","wall_time":"90"}`,
	})
	makeOld(t, eventsPath)

	e := New("fallout4", eventsPath, offsetPath, dir, q)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	item, ok := q.Peek()
	if !ok {
		t.Fatal("nothing queued")
	}
	if item.Capture.GapBefore != nil {
		t.Errorf("gap_before = %d across a counter reset; want omitted", *item.Capture.GapBefore)
	}
	// The span must not absorb the reset as negative or as a huge jump: only
	// the forward steps count.
	if item.Capture.SpanS != 87 {
		t.Errorf("span_s = %d, want 87 (the forward steps only)", item.Capture.SpanS)
	}
}

// TestExtract_NumericWallTimeDoesNotDropEvents guards the decode. wall_time is
// quoted in both shipping mods, but a mod emitting it bare must not have its
// events dropped: a strict string field would fail the whole envelope
// unmarshal, and a failed unmarshal skips the event entirely.
func TestExtract_NumericWallTimeDoesNotDropEvents(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	offsetPath := filepath.Join(dir, "offset")
	q := newTestQueue(t)

	writeEvents(t, eventsPath, []string{
		`{"type":"session_start","wall_time":100}`,
		`{"type":"kill","wall_time":250}`,
	})
	makeOld(t, eventsPath)

	e := New("aoe2", eventsPath, offsetPath, dir, q)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	item, ok := q.Peek()
	if !ok {
		t.Fatal("nothing queued")
	}
	if got := len(queuedLines(t, item)); got != 2 {
		t.Errorf("uploaded %d events, want 2 — a bare numeric wall_time dropped events", got)
	}
	if item.Capture.SpanS != 150 {
		t.Errorf("span_s = %d, want 150", item.Capture.SpanS)
	}
}
