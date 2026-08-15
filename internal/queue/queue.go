package queue

import (
	"encoding/json"
	"os"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/ludotrace/client/internal/capture"
)

type Item struct {
	GameID    string `json:"game_id"`
	TmpPath   string `json:"tmp_path"`
	EndOffset int64  `json:"end_offset"`
	Attempts  int    `json:"attempts"`
	// CapturedAt is when the session was extracted and enqueued. It is the
	// basis for both age-based eviction (drop-oldest past the max age) and
	// newest-first upload selection — see EvictExpired and PeekNewest. Persisted
	// so ordering and eviction survive a process restart from queue.json rather
	// than depending on volatile filesystem mtimes.
	CapturedAt time.Time `json:"captured_at"`
	// Capture is how the extractor captured this region (core#116) — sent with
	// the upload so the model can tell a partial run from a complete one. It is
	// persisted alongside the item because it describes bytes already written to
	// the temp file and cannot be recomputed once the events file has moved on.
	// Nil for an item enqueued before this field existed; the upload then omits
	// the form field and Core leaves the block out, exactly as before.
	Capture *capture.Context `json:"capture,omitempty"`
}

type Queue struct {
	path  string
	items []Item
	mu    sync.Mutex

	// enqueuedCh carries a coalesced signal that the queue went from empty to
	// having work, so a consumer can block instead of polling Len. Buffered
	// depth 1 with a non-blocking send: a signal raised while the consumer is
	// busy elsewhere is retained rather than lost, and repeated enqueues
	// collapse into the one wake they warrant.
	enqueuedCh chan struct{}
}

func New(path string) (*Queue, error) {
	q := &Queue{path: path, enqueuedCh: make(chan struct{}, 1)}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		q.items = []Item{}
		return q, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &q.items); err != nil {
		return nil, err
	}
	// Migrate a queue.json written before Item.CapturedAt existed: any item
	// loaded without a timestamp is backfilled so age-based eviction and
	// newest-first selection have something to work with. Best-effort persist;
	// on flush failure the same backfill just recomputes on the next load.
	if q.backfillTimestamps() {
		_ = q.flush()
	}
	return q, nil
}

// backfillTimestamps assigns a CapturedAt to any item that loaded without one
// (a legacy queue.json). It uses the temp file's mtime as an age proxy so a
// genuinely old queued session still ages out on schedule, falling back to
// now() when the file is gone. Returns true if any item was changed.
func (q *Queue) backfillTimestamps() bool {
	changed := false
	for i := range q.items {
		if !q.items[i].CapturedAt.IsZero() {
			continue
		}
		ts := time.Now()
		if fi, err := os.Stat(q.items[i].TmpPath); err == nil {
			ts = fi.ModTime()
		}
		q.items[i].CapturedAt = ts
		changed = true
	}
	return changed
}

func (q *Queue) flush() error {
	data, err := json.Marshal(q.items)
	if err != nil {
		return err
	}
	tmp := q.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, q.path)
}

func (q *Queue) Enqueue(item Item) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, item)
	// Signal before flushing, and regardless of its outcome: the item is in
	// q.items either way, so it is uploadable and a consumer blocked on
	// Enqueued must not be left asleep by a failed write to disk.
	q.signalEnqueued()
	return q.flush()
}

// Enqueued returns a channel that receives when an item is enqueued. A
// consumer that finds the queue empty blocks on this instead of re-checking
// Len on a timer — nothing but Enqueue can make the queue non-empty at
// runtime, so there is nothing a poll would find that this does not deliver.
// Items already on disk are loaded by New, before any consumer's first pass.
func (q *Queue) Enqueued() <-chan struct{} {
	return q.enqueuedCh
}

// signalEnqueued wakes a consumer blocked on Enqueued. Must be called with
// q.mu held. Non-blocking: a pending signal has not been consumed yet, and one
// wake is enough to get the consumer back to reading Len.
func (q *Queue) signalEnqueued() {
	select {
	case q.enqueuedCh <- struct{}{}:
	default:
	}
}

// Items returns a snapshot of the queued items in arrival order (oldest
// enqueued first). The returned slice is a copy: mutating it does not affect
// the queue. It exists for inspection — tests and diagnostics — not for
// selecting work: the upload worker selects with PeekNewest and retires by
// TmpPath (Remove), never positionally.
func (q *Queue) Items() []Item {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.items)
}

// PeekNewest returns the item with the most recent CapturedAt without removing
// it — the upload worker's selection order (process newest first). This is
// deliberately distinct from eviction order: EvictExpired drops oldest-past-
// threshold from the tail, while uploads are served newest-first so a
// chronically over-quota player spends their quota budget on the session they
// just played, not an ever-staler backlog. On a CapturedAt tie the earliest-
// enqueued of the tied items wins (stable). Returns false on an empty queue.
func (q *Queue) PeekNewest() (Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return Item{}, false
	}
	newest := 0
	for i := 1; i < len(q.items); i++ {
		if q.items[i].CapturedAt.After(q.items[newest].CapturedAt) {
			newest = i
		}
	}
	return q.items[newest], true
}

// EvictExpired removes every item whose age (now - CapturedAt) strictly exceeds
// maxAge and returns the evicted items in oldest-first order, so the caller can
// clean up each temp file and report the count. An item at exactly maxAge is
// kept (the boundary is inclusive of the threshold, exclusive of eviction).
// maxAge <= 0 disables eviction. Surviving items keep their relative order.
// The single flush persists the eviction so it is not re-derived after a
// restart. On flush failure nothing is reported evicted (items are retained in
// memory) so the caller never deletes a temp file the queue still references.
func (q *Queue) EvictExpired(maxAge time.Duration, now time.Time) ([]Item, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if maxAge <= 0 || len(q.items) == 0 {
		return nil, nil
	}
	kept := make([]Item, 0, len(q.items))
	var expired []Item
	for _, it := range q.items {
		if now.Sub(it.CapturedAt) > maxAge {
			expired = append(expired, it)
		} else {
			kept = append(kept, it)
		}
	}
	if len(expired) == 0 {
		return nil, nil
	}
	sort.Slice(expired, func(i, j int) bool {
		return expired[i].CapturedAt.Before(expired[j].CapturedAt)
	})
	prev := q.items
	q.items = kept
	if err := q.flush(); err != nil {
		q.items = prev // roll back so we don't lose track of the items
		return nil, err
	}
	return expired, nil
}

func (q *Queue) Contains(tmpPath string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, item := range q.items {
		if item.TmpPath == tmpPath {
			return true
		}
	}
	return false
}

func (q *Queue) UpdateAttempts(tmpPath string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].TmpPath == tmpPath {
			q.items[i].Attempts++
			return q.flush()
		}
	}
	return nil
}

func (q *Queue) Remove(tmpPath string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	origLen := len(q.items)
	filtered := make([]Item, 0, origLen)
	for _, item := range q.items {
		if item.TmpPath != tmpPath {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == origLen {
		return nil
	}
	q.items = filtered
	return q.flush()
}

func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}
