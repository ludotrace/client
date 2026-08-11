package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ludotrace/client/internal/queue"
)

// orphanThreshold is how long the events file must go unwritten before an open
// session is treated as finished. It is the single source of truth for the
// inactivity boundary; the docs describe this value, not a separate one.
const orphanThreshold = 12 * time.Minute

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
	lines [][]byte
}

func (s *openSession) append(rawJSON []byte) {
	lineCopy := make([]byte, len(rawJSON))
	copy(lineCopy, rawJSON)
	s.lines = append(s.lines, lineCopy)
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

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("session: stat: %w", err)
	}
	modTime := fi.ModTime()

	// A file shorter than the stored offset was replaced, not appended to —
	// the mod was reinstalled, the file deleted and recreated, or its name
	// changed. Seeking past EOF succeeds silently and reads nothing, which
	// would strand the game forever: no bytes read, so the offset never
	// advances, so nothing ever uploads again. Restart from zero instead.
	if fi.Size() < offset {
		slog.Warn("session: events file shorter than stored offset; restarting from zero",
			"game_id", e.gameID, "path", e.eventsPath, "size", fi.Size(), "offset", offset)
		offset = 0
	}

	if _, err := f.Seek(offset, 0); err != nil {
		return fmt.Errorf("session: seek: %w", err)
	}

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

		// The Client recognises exactly one structural boundary marker —
		// session_start — plus the inactivity timeout below. Everything else,
		// including session_end, is opaque payload buffered into the open
		// session. This keeps the boundary logic game-agnostic: games emit
		// session_end on different cadences (Fallout 4, for example, writes one
		// per save, so a single play session contains many), and the Client
		// must not treat any of them as a terminator or it would split or drop
		// data based on a game-specific quirk. A play session therefore runs
		// from one session_start to the next, or to a gap in activity.
		if env.Type == "session_start" {
			// A new session beginning means any session still open has ended —
			// the game was reloaded. Flush it (rather than discard it) up to the
			// byte where this session_start begins, then open the new one.
			if current != nil {
				if err := e.writeAndEnqueue(current, curOffset); err != nil {
					return err
				}
			}
			current = &openSession{}
			current.append(rawJSON)
		} else if current != nil {
			current.append(rawJSON)
		}

		curOffset += lineLen
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("session: scan: %w", err)
	}

	// Inactivity boundary: an open session whose events file hasn't been
	// modified in >orphanThreshold is treated as finished and flushed. This is
	// the generic "the player stopped" signal — it is what closes the final
	// session of a play period, including one that ended without a clean
	// session_end (e.g. a crash or a quit with no final save).
	if current != nil && time.Since(modTime) > orphanThreshold {
		if err := e.writeAndEnqueue(current, curOffset); err != nil {
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
		GameID:     e.gameID,
		TmpPath:    tmpPath,
		EndOffset:  endOffset,
		CapturedAt: time.Now(),
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
