package session

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ludotrace/client/internal/queue"
)

// BenchmarkExtract builds an events file of the given size where the sidecar
// offset points near the end, then measures Extract(). Since Extract seeks to
// the offset before scanning, cost should stay flat across file sizes
// (bounded by the unread tail, not the whole file) rather than growing
// linearly with total file size.
func BenchmarkExtract(b *testing.B) {
	for _, sizeMB := range []int{1, 10} {
		b.Run(fmt.Sprintf("%dMB", sizeMB), func(b *testing.B) {
			dir := b.TempDir()
			eventsPath := filepath.Join(dir, "events.jsonl")
			offsetPath := filepath.Join(dir, "offset")

			target := sizeMB << 20
			line := []byte(`{"type":"kill","target":"radroach"}` + "\n")

			var buf bytes.Buffer
			for buf.Len() < target {
				buf.Write(line)
			}
			offset := int64(buf.Len())

			// Tail after the offset: what Extract actually has to read and process.
			buf.WriteString(`{"type":"session_start","session_id":"tail"}` + "\n")
			for i := 0; i < 5; i++ {
				buf.Write(line)
			}

			if err := os.WriteFile(eventsPath, buf.Bytes(), 0o600); err != nil {
				b.Fatalf("write events: %v", err)
			}
			if err := os.WriteFile(offsetPath, []byte(strconv.FormatInt(offset, 10)), 0o600); err != nil {
				b.Fatalf("write offset: %v", err)
			}

			q, err := queue.New(filepath.Join(dir, "queue.json"))
			if err != nil {
				b.Fatalf("queue.New: %v", err)
			}
			e := New("bench", eventsPath, offsetPath, dir, q)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := e.Extract(); err != nil {
					b.Fatalf("Extract: %v", err)
				}
			}
		})
	}
}
