package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ludotrace/client/internal/queue"
	"github.com/ludotrace/client/internal/session"
)

// retireItem is what both terminal outcomes (202, permanent rejection) run. It
// must retire the item it was handed — not the head of the queue — and leave
// nothing of that item behind: no queue entry, no temp file, no unadvanced
// offset that would have Extract() rebuild it on the next write event.
func TestRetireItem_RetiresTheNamedItemOnly(t *testing.T) {
	dir := t.TempDir()

	q, err := queue.New(filepath.Join(dir, "queue.json"))
	if err != nil {
		t.Fatalf("queue.New: %v", err)
	}

	offsetPath := filepath.Join(dir, "fallout4.offset")
	extractors := newExtractorStore()
	extractors.set("fallout4", session.New(
		"fallout4", filepath.Join(dir, "events.jsonl"), offsetPath, dir, q))

	now := time.Now()
	older := queue.Item{
		GameID: "fallout4", TmpPath: filepath.Join(dir, "lt_session_older.jsonl"),
		EndOffset: 200, CapturedAt: now.Add(-time.Hour),
	}
	newer := queue.Item{
		GameID: "fallout4", TmpPath: filepath.Join(dir, "lt_session_newer.jsonl"),
		EndOffset: 500, CapturedAt: now,
	}
	for _, it := range []queue.Item{older, newer} {
		if werr := os.WriteFile(it.TmpPath, []byte("{}\n"), 0600); werr != nil {
			t.Fatalf("write temp file: %v", werr)
		}
		if eerr := q.Enqueue(it); eerr != nil {
			t.Fatalf("Enqueue: %v", eerr)
		}
	}

	// The worker selects with PeekNewest, so this is the item a permanent
	// failure would hand back — and it is not the head of the queue.
	selected, ok := q.PeekNewest()
	if !ok || selected.TmpPath != newer.TmpPath {
		t.Fatalf("PeekNewest = %+v, want the newer item", selected)
	}

	retireItem(q, extractors, selected)

	if q.Len() != 1 {
		t.Fatalf("queue length = %d, want 1", q.Len())
	}
	if q.Contains(newer.TmpPath) {
		t.Error("retired item is still queued — it will be uploaded again")
	}
	if !q.Contains(older.TmpPath) {
		t.Error("the older item was retired instead; that session is lost unsent")
	}

	if _, serr := os.Stat(newer.TmpPath); !os.IsNotExist(serr) {
		t.Errorf("retired temp file still present (stat err %v) — it leaks", serr)
	}
	if _, serr := os.Stat(older.TmpPath); serr != nil {
		t.Errorf("temp file of a still-queued item was deleted: %v", serr)
	}

	// The offset must clear the retired region, or Extract() rebuilds it.
	data, rerr := os.ReadFile(offsetPath)
	if rerr != nil {
		t.Fatalf("read offset file: %v", rerr)
	}
	if got := string(data); got != "500\n" {
		t.Errorf("offset = %q, want %q", got, "500\n")
	}
}
