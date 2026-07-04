package queue

import (
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkQueueFlush measures the atomic write+rename cost of flush() with a
// queue depth (50 items) typical of a burst of extracted sessions, to catch
// serialization overhead regressions before they show up as upload lag.
func BenchmarkQueueFlush(b *testing.B) {
	dir := b.TempDir()
	q, err := New(filepath.Join(dir, "queue.json"))
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	for i := 0; i < 50; i++ {
		q.items = append(q.items, Item{
			GameID:    "fallout4",
			TmpPath:   fmt.Sprintf("/tmp/lt_session_%d.jsonl", i),
			EndOffset: int64(i * 1024),
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := q.flush(); err != nil {
			b.Fatalf("flush: %v", err)
		}
	}
}
