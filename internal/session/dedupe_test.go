package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func countTempFiles(t *testing.T, dir string) int {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "lt_session_*.jsonl"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	return len(m)
}

// While uploads are blocked the offset never advances, so every pass re-cuts
// the same sessions. The queue must hold each region once, and the redundant
// temp files must not pile up (client#95).
func TestExtract_RepeatedPassesDoNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	q := newTestQueue(t)

	writeEvents(t, eventsPath, []string{
		`{"type":"session_start","session_id":"s1"}`,
		`{"type":"kill","target":"radroach"}`,
		`{"type":"session_start","session_id":"s2"}`,
		`{"type":"kill","target":"mirelurk"}`,
	})
	makeOld(t, eventsPath)

	e := New("fallout4", eventsPath, filepath.Join(dir, "offset"), dir, q)
	for i := 0; i < 5; i++ {
		if err := e.Extract(); err != nil {
			t.Fatalf("Extract pass %d: %v", i, err)
		}
	}

	if q.Len() != 2 {
		t.Fatalf("queue length = %d, want 2", q.Len())
	}
	if n := countTempFiles(t, dir); n != 2 {
		t.Fatalf("temp files = %d, want 2", n)
	}
}

// An inactivity cut queued during a pause is a prefix of the session that
// resumes after it. Once the longer cut is queued, the shorter one is
// redundant and must be replaced, not uploaded alongside it.
func TestExtract_LongerCutSupersedesIdleCut(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "events.jsonl")
	q := newTestQueue(t)

	first := []string{
		`{"type":"session_start","session_id":"s1"}`,
		`{"type":"kill","target":"radroach"}`,
	}
	writeEvents(t, eventsPath, first)
	makeOld(t, eventsPath)

	e := New("fallout4", eventsPath, filepath.Join(dir, "offset"), dir, q)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("after idle cut: queue length = %d, want 1", q.Len())
	}
	idleCut := q.Items()[0]

	writeEvents(t, eventsPath, append(first, `{"type":"kill","target":"ghoul"}`))
	makeOld(t, eventsPath)
	if err := e.Extract(); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	items := q.Items()
	if len(items) != 1 {
		t.Fatalf("queue length = %d, want 1", len(items))
	}
	if items[0].EndOffset <= idleCut.EndOffset {
		t.Fatalf("kept EndOffset %d, want the longer cut (> %d)", items[0].EndOffset, idleCut.EndOffset)
	}
	if _, err := os.Stat(idleCut.TmpPath); !os.IsNotExist(err) {
		t.Fatalf("superseded temp file still present (err=%v)", err)
	}
	data, err := os.ReadFile(items[0].TmpPath)
	if err != nil {
		t.Fatalf("read kept temp file: %v", err)
	}
	if !strings.Contains(string(data), "ghoul") {
		t.Fatalf("kept temp file lacks the resumed events: %q", data)
	}
}
