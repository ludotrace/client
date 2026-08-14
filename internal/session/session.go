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

	"github.com/ludotrace/client/internal/capture"
	"github.com/ludotrace/client/internal/queue"
)

// orphanThreshold is how long the events file must go unwritten before an open
// session is treated as finished. It is the single source of truth for the
// inactivity boundary; the docs describe this value, not a separate one.
const orphanThreshold = 12 * time.Minute

// holdCap bounds the carried-forward lead-in (see Extract). Held bytes are not
// a session and are waiting for one, so nothing else would ever release them;
// this is what stops a run that never emits another session_start from holding
// without limit. It is a raw-bytes figure and uploads are gzipped, so it stays
// far clear of Core's 5 MiB body limit.
const holdCap = 4 << 20

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

// AdvanceOffset moves the read position forward as a session is retired. It
// never moves it backward: items are retired newest-first (main.go's
// PeekNewest), so a pass that enqueued two sessions can report the later
// endOffset before the earlier one. Writing that earlier value would re-expose
// bytes already sent, and Core does not dedupe — a re-upload is a second job, a
// second LLM run, and another decrement of the user's upload limit.
//
// The comparison reads the file rather than caching in memory, so a
// hand-edited offset stays authoritative and still rewinds. Extract() relies on
// that too: when the events file is replaced by a shorter one it resets the
// stored offset directly, because every endOffset the new file can produce is
// below the old high-water mark and would be refused here.
func (e *Extractor) AdvanceOffset(offset int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	current, err := e.readOffset()
	if err != nil {
		return fmt.Errorf("session: read offset: %w", err)
	}
	if offset <= current {
		slog.Debug("session: ignoring non-advancing offset",
			"game_id", e.gameID, "current", current, "requested", offset)
		return nil
	}

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
	Type     string          `json:"type"`
	WallTime json.RawMessage `json:"wall_time"`
}

// openSession is a region of the events file being accumulated for one upload.
// Alongside the bytes it tracks what Core's capture context needs (core#116):
// how many events went in, how much play time they span, and — once the region
// is closed — whether its opening session_start is inside it.
type openSession struct {
	lines [][]byte
	bytes int

	// opener is set when the region is created, from whether it began at a
	// session_start or at carried-forward bytes.
	opener string
	// gapBefore is the idle between a carried-forward lead-in and the session
	// it leads into. Nil unless this region has a lead-in and the gap was
	// measurable.
	gapBefore *int

	// last is the most recent parseable wall_time seen, and spanS the sum of
	// the forward steps between consecutive readings. Summing steps rather
	// than subtracting first from last keeps the span honest across a game
	// restart, where a relative counter resets and the raw difference goes
	// negative.
	last  wallClock
	spanS int
}

func (s *openSession) append(rawJSON []byte, clock wallClock) {
	lineCopy := make([]byte, len(rawJSON))
	copy(lineCopy, rawJSON)
	s.lines = append(s.lines, lineCopy)
	s.bytes += len(lineCopy) + 1 // +1 for the newline writeAndEnqueue adds

	if clock.kind == clockNone {
		return
	}
	if d, ok := delta(s.last, clock); ok {
		s.spanS += d
	}
	s.last = clock
}

// leadInto turns a held lead-in into the opening of the session starting at
// clock. The held bytes keep their position at the front of the region — they
// are the same contiguous run of the file — and the region as a whole reports
// no opener, because the session_start that began the held bytes was flushed in
// an earlier upload.
func (s *openSession) leadInto(clock wallClock) {
	s.opener = capture.OpenerAbsent
	if d, ok := delta(s.last, clock); ok {
		s.gapBefore = &d
	}
}

// captureContext reports how the region was captured, given what closed it.
func (s *openSession) captureContext(closedBy string) *capture.Context {
	return &capture.Context{
		Opener:     s.opener,
		ClosedBy:   closedBy,
		GapBefore:  s.gapBefore,
		EventCount: len(s.lines),
		SpanS:      s.spanS,
	}
}

// Extract reads everything appended since the stored offset and enqueues
// whatever complete sessions it finds.
func (e *Extractor) Extract() error { return e.extract(false) }

// FlushForShutdown is Extract with the Client's own exit as a closing boundary:
// an open session is enqueued rather than left for an indefinite next launch.
// It is the Client's lifecycle, not observation of the game.
//
// A held lead-in is deliberately *not* released here. Shutdown says nothing
// about whether those bytes will gain an opener — they keep their place on
// disk with the offset unmoved, and the next launch picks them up unchanged.
func (e *Extractor) FlushForShutdown() error { return e.extract(true) }

func (e *Extractor) extract(shutdown bool) error {
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
	//
	// The reset is written through to the sidecar, not just held for this pass.
	// AdvanceOffset refuses anything at or below the stored value, and every
	// endOffset the shorter file can produce is below the old mark — so leaving
	// the mark in place would have each pass re-extract, re-enqueue and
	// re-upload the same sessions indefinitely.
	if fi.Size() < offset {
		slog.Warn("session: events file shorter than stored offset; restarting from zero",
			"game_id", e.gameID, "path", e.eventsPath, "size", fi.Size(), "offset", offset)
		offset = 0
		if werr := writeOffsetFile(e.offsetPath, 0); werr != nil {
			return fmt.Errorf("session: reset offset: %w", werr)
		}
	}

	if _, err := f.Seek(offset, 0); err != nil {
		return fmt.Errorf("session: seek: %w", err)
	}

	scanner := bufio.NewScanner(f)
	// scanRawLines keeps \r in the token so lineLen is accurate for \r\n files.
	// The default ScanLines strips \r silently, causing 1-byte drift per line.
	scanner.Split(scanRawLines)

	var current *openSession

	// leadIn holds bytes that arrived with no session open — the region between
	// the stored offset and the first session_start. They are not a session and
	// never become one on their own: an earlier flush already uploaded their
	// opener, so uploading them alone would spend an inference run on a
	// fragment the model cannot place. They are held instead, with the offset
	// unmoved so they stay on disk and survive a restart, and are prepended to
	// the next session as its lead-in.
	//
	// Before this, such bytes were discarded outright (ludotrace/client#75): the
	// offset only advances on a successful upload, so a run whose opener had
	// been flushed by the inactivity timeout could never upload anything again.
	var leadIn *openSession

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
		clock := parseWallTime(env.WallTime)

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
				if err := e.writeAndEnqueue(current, curOffset, capture.ClosedBySuperseded); err != nil {
					return err
				}
				current = nil
			}

			// A held lead-in has just gained the session it leads into. It is
			// released as that session's opening bytes, in place: one
			// contiguous region from the offset, uploaded as one unit.
			if leadIn != nil {
				current = leadIn
				current.leadInto(clock)
				leadIn = nil
			} else {
				current = &openSession{opener: capture.OpenerPresent}
			}
			current.append(rawJSON, clock)
		} else if current != nil {
			current.append(rawJSON, clock)
		} else {
			if leadIn == nil {
				leadIn = &openSession{opener: capture.OpenerAbsent}
			}
			leadIn.append(rawJSON, clock)
		}

		curOffset += lineLen
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("session: scan: %w", err)
	}

	// Two closing boundaries, both from what the Client already knows.
	//
	// Inactivity: the events file hasn't been modified in >orphanThreshold, the
	// generic "the player stopped" signal. It is what closes the final session
	// of a play period, including one that ended without a clean session_end
	// (e.g. a crash or a quit with no final save).
	//
	// Shutdown: the Client is exiting, so what is coherent now is enqueued now.
	if current != nil {
		switch {
		case shutdown:
			if err := e.writeAndEnqueue(current, curOffset, capture.ClosedByClientShutdown); err != nil {
				return err
			}
		case time.Since(modTime) > orphanThreshold:
			if err := e.writeAndEnqueue(current, curOffset, capture.ClosedByIdleTimeout); err != nil {
				return err
			}
		}
		return nil
	}

	// Nothing opened a session over the whole region, so everything read is
	// held. Holding is close to lossless — the bytes stay on disk and the offset
	// does not move — but it cannot grow without bound, so a hold past holdCap
	// is released on its own.
	//
	// It is released rather than dropped: a fragment that large is
	// unambiguously substantial, which is what makes it worth an inference run
	// where a two-minute one would not be. It is cut mid-play, so it reports
	// size_cap and never the inactivity or shutdown closers.
	if leadIn != nil && leadIn.bytes > holdCap {
		slog.Info("session: releasing carried-forward lead-in at hold cap",
			"game_id", e.gameID, "bytes", leadIn.bytes, "events", len(leadIn.lines))
		if err := e.writeAndEnqueue(leadIn, curOffset, capture.ClosedBySizeCap); err != nil {
			return err
		}
	}

	return nil
}

func (e *Extractor) writeAndEnqueue(sess *openSession, endOffset int64, closedBy string) error {
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
		Capture:    sess.captureContext(closedBy),
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
