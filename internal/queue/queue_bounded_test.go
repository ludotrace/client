package queue

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

const day = 24 * time.Hour

// enqueueAged is a helper that enqueues an item captured ageDays before now.
func enqueueAged(t *testing.T, q *Queue, tmpPath string, ageDays int, now time.Time) {
	t.Helper()
	if err := q.Enqueue(Item{
		TmpPath:    tmpPath,
		CapturedAt: now.Add(-time.Duration(ageDays) * day),
	}); err != nil {
		t.Fatalf("enqueue %s: %v", tmpPath, err)
	}
}

func tmpPaths(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.TmpPath
	}
	return out
}

func TestEvictExpired_DropsOldestFirst(t *testing.T) {
	now := time.Now()
	q, _ := New(queuePath(t))
	// Arrival order is deliberately not age order, to prove eviction is by age.
	enqueueAged(t, q, "/tmp/mid", 40, now)  // expired
	enqueueAged(t, q, "/tmp/fresh", 1, now) // kept
	enqueueAged(t, q, "/tmp/old", 90, now)  // expired (oldest)
	enqueueAged(t, q, "/tmp/edge", 20, now) // kept

	dropped, err := q.EvictExpired(30*day, now)
	if err != nil {
		t.Fatalf("EvictExpired: %v", err)
	}
	// Two expired, returned oldest-first (by CapturedAt): /tmp/old then /tmp/mid.
	got := tmpPaths(dropped)
	want := []string{"/tmp/old", "/tmp/mid"}
	if len(got) != len(want) {
		t.Fatalf("dropped count = %d (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dropped[%d] = %s, want %s (full: %v)", i, got[i], want[i], got)
		}
	}
	if q.Len() != 2 {
		t.Fatalf("remaining = %d, want 2", q.Len())
	}
	// Survivors keep their relative arrival order.
	if got := tmpPaths(q.Items()); got[0] != "/tmp/fresh" || got[1] != "/tmp/edge" {
		t.Errorf("survivor order = %v, want [/tmp/fresh /tmp/edge]", got)
	}
}

func TestEvictExpired_ThresholdBoundary(t *testing.T) {
	now := time.Now()
	maxAge := 30 * day

	q, _ := New(queuePath(t))
	// Exactly at the threshold: age == maxAge → kept (eviction is strictly >).
	if err := q.Enqueue(Item{TmpPath: "/tmp/exact", CapturedAt: now.Add(-maxAge)}); err != nil {
		t.Fatal(err)
	}
	// One nanosecond past the threshold → evicted.
	if err := q.Enqueue(Item{TmpPath: "/tmp/past", CapturedAt: now.Add(-maxAge - time.Nanosecond)}); err != nil {
		t.Fatal(err)
	}

	dropped, err := q.EvictExpired(maxAge, now)
	if err != nil {
		t.Fatalf("EvictExpired: %v", err)
	}
	if len(dropped) != 1 || dropped[0].TmpPath != "/tmp/past" {
		t.Fatalf("dropped = %v, want exactly [/tmp/past]", tmpPaths(dropped))
	}
	if !q.Contains("/tmp/exact") {
		t.Error("item exactly at threshold was evicted; boundary should be inclusive-keep")
	}
}

func TestEvictExpired_DisabledOrEmpty(t *testing.T) {
	now := time.Now()
	q, _ := New(queuePath(t))
	enqueueAged(t, q, "/tmp/ancient", 999, now)

	// maxAge <= 0 disables eviction entirely.
	dropped, err := q.EvictExpired(0, now)
	if err != nil {
		t.Fatalf("EvictExpired(0): %v", err)
	}
	if len(dropped) != 0 || q.Len() != 1 {
		t.Fatalf("maxAge=0 evicted %d (len now %d); want none", len(dropped), q.Len())
	}

	// Empty queue is a no-op.
	q2, _ := New(queuePath(t))
	dropped, err = q2.EvictExpired(30*day, now)
	if err != nil || len(dropped) != 0 {
		t.Fatalf("empty-queue evict: dropped=%v err=%v", tmpPaths(dropped), err)
	}
}

func TestEvictExpired_PersistsFlush(t *testing.T) {
	now := time.Now()
	path := queuePath(t)
	q, _ := New(path)
	enqueueAged(t, q, "/tmp/old", 90, now)
	enqueueAged(t, q, "/tmp/fresh", 1, now)

	if _, err := q.EvictExpired(30*day, now); err != nil {
		t.Fatalf("EvictExpired: %v", err)
	}
	// Reload from disk: the eviction must be persisted, not just in-memory.
	q2, err := New(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if q2.Len() != 1 || q2.Contains("/tmp/old") {
		t.Fatalf("post-restart queue = %d items, contains old=%v; eviction not persisted",
			q2.Len(), q2.Contains("/tmp/old"))
	}
}

func TestPeekNewest_SelectsMostRecent(t *testing.T) {
	now := time.Now()
	q, _ := New(queuePath(t))
	enqueueAged(t, q, "/tmp/old", 10, now)
	enqueueAged(t, q, "/tmp/newest", 1, now)
	enqueueAged(t, q, "/tmp/mid", 5, now)

	got, ok := q.PeekNewest()
	if !ok {
		t.Fatal("PeekNewest returned false on non-empty queue")
	}
	if got.TmpPath != "/tmp/newest" {
		t.Errorf("PeekNewest = %s, want /tmp/newest", got.TmpPath)
	}
	// Peek must not remove.
	if q.Len() != 3 {
		t.Errorf("PeekNewest removed an item; Len=%d", q.Len())
	}
}

func TestPeekNewest_Empty(t *testing.T) {
	q, _ := New(queuePath(t))
	if _, ok := q.PeekNewest(); ok {
		t.Fatal("expected false from PeekNewest on empty queue")
	}
}

// TestBoundedQueue_SurvivesRestart proves that both derived orderings —
// newest-first selection and age-based eviction — read from persisted
// CapturedAt in queue.json, not volatile in-memory state.
func TestBoundedQueue_SurvivesRestart(t *testing.T) {
	now := time.Now()
	path := queuePath(t)

	q1, _ := New(path)
	enqueueAged(t, q1, "/tmp/ancient", 90, now) // will evict after restart
	enqueueAged(t, q1, "/tmp/old", 10, now)     // survives; not newest
	enqueueAged(t, q1, "/tmp/newest", 2, now)   // survives; newest

	// Simulate a process restart: brand-new Queue from the same file.
	q2, err := New(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	dropped, err := q2.EvictExpired(30*day, now)
	if err != nil {
		t.Fatalf("EvictExpired after restart: %v", err)
	}
	if len(dropped) != 1 || dropped[0].TmpPath != "/tmp/ancient" {
		t.Fatalf("post-restart eviction = %v, want [/tmp/ancient]", tmpPaths(dropped))
	}
	newest, ok := q2.PeekNewest()
	if !ok || newest.TmpPath != "/tmp/newest" {
		t.Fatalf("post-restart PeekNewest = %v (ok=%v), want /tmp/newest", newest.TmpPath, ok)
	}
}

// TestBackfillLegacyTimestamps covers migrating a queue.json written before
// Item.CapturedAt existed: items load with a zero timestamp and must be
// backfilled (and persisted) rather than instantly treated as ancient.
func TestBackfillLegacyTimestamps(t *testing.T) {
	path := queuePath(t)
	// A pre-CapturedAt queue.json: no captured_at key at all.
	legacy := `[{"game_id":"fo4","tmp_path":"/tmp/legacy","end_offset":10,"attempts":0}]`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	q, err := New(path)
	if err != nil {
		t.Fatalf("New on legacy file: %v", err)
	}
	loaded := q.Items()
	if len(loaded) != 1 {
		t.Fatalf("legacy item missing after load: %v", tmpPaths(loaded))
	}
	if loaded[0].CapturedAt.IsZero() {
		t.Error("legacy item CapturedAt not backfilled")
	}

	// The backfill must be persisted so it is stable across restarts.
	var onDisk []Item
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("re-read queue.json: %v", err)
	}
	if len(onDisk) != 1 || onDisk[0].CapturedAt.IsZero() {
		t.Errorf("backfill not persisted to disk: %+v", onDisk)
	}
}
