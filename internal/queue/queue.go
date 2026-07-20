package queue

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
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
}

type Queue struct {
	path  string
	items []Item
	mu    sync.Mutex
}

func New(path string) (*Queue, error) {
	q := &Queue{path: path}
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
	return q.flush()
}

func (q *Queue) Peek() (Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return Item{}, false
	}
	return q.items[0], true
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

func (q *Queue) Dequeue() (Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return Item{}, false
	}
	item := q.items[0]
	q.items = q.items[1:]
	if err := q.flush(); err != nil {
		// re-insert to keep in-memory state consistent with what caller expects
		q.items = append([]Item{item}, q.items...)
		return Item{}, false
	}
	return item, true
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
