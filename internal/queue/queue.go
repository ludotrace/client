package queue

import (
	"encoding/json"
	"os"
	"sync"
)

type Item struct {
	GameID    string `json:"game_id"`
	TmpPath   string `json:"tmp_path"`
	EndOffset int64  `json:"end_offset"`
	Attempts  int    `json:"attempts"`
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
	return q, nil
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
