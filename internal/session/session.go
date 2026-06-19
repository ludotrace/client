package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ludotrace/client/internal/queue"
)

const orphanThreshold = 30 * time.Minute

type Extractor struct {
	gameID     string
	eventsPath string
	offsetPath string
	tempDir    string
	q          *queue.Queue
	mu         sync.Mutex
}

func New(gameID, eventsPath, offsetPath, tempDir string, q *queue.Queue) *Extractor {
	return &Extractor{
		gameID:     gameID,
		eventsPath: eventsPath,
		offsetPath: offsetPath,
		tempDir:    tempDir,
		q:          q,
	}
}

func (e *Extractor) readOffset() (int64, error) {
	data, err := os.ReadFile(e.offsetPath)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0, nil
	}
	offset, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// corrupt offset file; start from zero rather than failing the watcher loop
		return 0, nil
	}
	return offset, nil
}

func (e *Extractor) AdvanceOffset(offset int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return writeOffsetFile(e.offsetPath, offset)
}

func writeOffsetFile(path string, offset int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	content := fmt.Sprintf("%d\n", offset)
	if err := os.WriteFile(tmp, []byte(content), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type eventEnvelope struct {
	Type string `json:"type"`
}

type openSession struct {
	lines     [][]byte
	startOff  int64 // byte offset of the session_start line
	lastLineAt time.Time
}

func (e *Extractor) Extract() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	offset, err := e.readOffset()
	if err != nil {
		return fmt.Errorf("session: read offset: %w", err)
	}

	f, err := os.Open(e.eventsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("session: open events: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(offset, 0); err != nil {
		return fmt.Errorf("session: seek: %w", err)
	}

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("session: stat: %w", err)
	}
	modTime := fi.ModTime()

	scanner := bufio.NewScanner(f)
	// scanRawLines keeps \r in the token so lineLen is accurate for \r\n files.
	// The default ScanLines strips \r silently, causing 1-byte drift per line.
	scanner.Split(scanRawLines)

	var current *openSession
	curOffset := offset // tracks byte position as we read

	for scanner.Scan() {
		raw := scanner.Bytes()
		lineLen := int64(len(raw)) + 1 // +1 for the \n that scanRawLines consumed

		rawJSON := bytes.TrimRight(raw, "\r")
		var env eventEnvelope
		if err := json.Unmarshal(rawJSON, &env); err != nil {
			// skip unparseable lines but still advance offset
			curOffset += lineLen
			continue
		}

		now := time.Now()
		switch env.Type {
		case "session_start":
			// Drop any previously open session that has no end — it's still live
			// (the game re-started a new session without ending the prior one cleanly).
			current = &openSession{
				startOff:   curOffset,
				lastLineAt: now,
			}
			lineCopy := make([]byte, len(rawJSON))
			copy(lineCopy, rawJSON)
			current.lines = append(current.lines, lineCopy)

		case "session_end":
			if current != nil {
				current.lastLineAt = now
				lineCopy := make([]byte, len(rawJSON))
				copy(lineCopy, rawJSON)
				current.lines = append(current.lines, lineCopy)
				endOffset := curOffset + lineLen
				if err := e.writeAndEnqueue(current, endOffset); err != nil {
					return err
				}
				current = nil
			}

		default:
			if current != nil {
				current.lastLineAt = now
				lineCopy := make([]byte, len(rawJSON))
				copy(lineCopy, rawJSON)
				current.lines = append(current.lines, lineCopy)
			}
		}

		curOffset += lineLen
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("session: scan: %w", err)
	}

	// Orphan detection: open session + events file hasn't been modified in >30min.
	if current != nil && time.Since(modTime) > orphanThreshold {
		endOffset := curOffset
		if err := e.writeAndEnqueue(current, endOffset); err != nil {
			return err
		}
	}

	return nil
}

func (e *Extractor) writeAndEnqueue(sess *openSession, endOffset int64) error {
	tmpFile, err := os.CreateTemp(e.tempDir, "lt_session_*.jsonl")
	if err != nil {
		return fmt.Errorf("session: create temp: %w", err)
	}
	tmpPath := tmpFile.Name()

	w := bufio.NewWriter(tmpFile)
	for _, line := range sess.lines {
		if _, err := w.Write(line); err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("session: write line: %w", err)
		}
		if err := w.WriteByte('\n'); err != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("session: write newline: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("session: flush: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("session: close temp: %w", err)
	}

	return e.q.Enqueue(queue.Item{
		GameID:    e.gameID,
		TmpPath:   tmpPath,
		EndOffset: endOffset,
	})
}

// scanRawLines is a bufio.SplitFunc that splits on \n but keeps \r in the
// token. The standard ScanLines strips \r silently, causing offset drift of
// 1 byte per line on Windows \r\n files.
func scanRawLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil // token includes \r if present; advance past \n
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
