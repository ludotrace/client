package session

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// wall_time is whatever the mod chose to put there.
//
// The capture layer is open — anyone can build a mod for any game — so this
// field arrives in whatever shape a given author found reachable from their
// modding API. The spec asks for an ISO 8601 UTC instant, but that is guidance
// to mod authors, not a guarantee to this code, and a Client that only handled
// the spec-perfect form would silently report nothing for everyone else.
//
// So: parse generously, interpret narrowly.
//
//   - Accept as many shapes as can be recognized without guessing.
//   - Only ever use *differences* between two readings, never an absolute value,
//     so an unknown origin or an unknown timezone costs nothing.
//   - Never subtract readings that are not evidently the same kind.
//   - When anything is unclear, report no measurement. An omitted gap_before is
//     honest; a fabricated one tells the model a boundary exists where none does.
//
// Nothing here should grow a heuristic that infers meaning from magnitude
// (guessing epoch-vs-counter, or seconds-vs-milliseconds, from how big a number
// looks). That trades a visible "unknown" for an invisible wrong answer.

// clockKind is how a reading has to be treated, not a taxonomy of mods. Two
// readings are comparable only when they share a kind.
type clockKind int

const (
	clockNone clockKind = iota
	// clockInstant is a reading that parsed as a calendar date and time. Its
	// origin is fixed, so two instants are comparable.
	clockInstant
	// clockCounter is a bare number of seconds from an origin the mod chose and
	// never states — time since launch, since load, since an epoch. The origin
	// is unknown and may reset, so only differences mean anything, and only
	// within a stretch where it did not reset.
	clockCounter
)

// wallClock is one parsed wall_time reading, held as seconds so both kinds
// share arithmetic.
type wallClock struct {
	kind    clockKind
	seconds float64
}

// instantLayouts are the date-time shapes recognized as a clockInstant, tried
// in order. The list is deliberately broad: separator, precision, and timezone
// vary between modding APIs, and none of that variation changes what a
// difference between two readings means.
//
// A layout without a zone parses as UTC, which is right for a difference as
// long as the mod is self-consistent — and a mod that switches timezone
// mid-file is beyond what any parsing can rescue.
var instantLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02T15:04:05Z0700",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	"2006/01/02 15:04:05",
	time.RFC1123Z,
	time.RFC1123,
	time.UnixDate,
	time.ANSIC,
}

// parseWallTime reads an event's wall_time field.
//
// It takes json.RawMessage rather than a string on purpose: decoding into a
// string would make a mod that emits a bare number fail to unmarshal, and a
// failed unmarshal drops the whole event. Timing detail is never worth losing
// an event over — which is the same reason every failure here yields clockNone
// instead of an error.
func parseWallTime(raw json.RawMessage) wallClock {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return wallClock{}
	}

	// A bare JSON number is a counter.
	if raw[0] != '"' {
		var n float64
		if err := json.Unmarshal(raw, &n); err == nil {
			return wallClock{kind: clockCounter, seconds: n}
		}
		return wallClock{}
	}

	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return wallClock{}
	}
	return parseWallTimeString(s)
}

// parseWallTimeString recognizes a quoted wall_time. Split out so the string
// forms can be exercised directly.
func parseWallTimeString(s string) wallClock {
	s = strings.TrimSpace(s)
	if s == "" {
		return wallClock{}
	}

	// A quoted number is the same counter as a bare one — several modding APIs
	// can only build strings, so the quoting says nothing about the value.
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return wallClock{kind: clockCounter, seconds: n}
	}

	for _, layout := range instantLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return wallClock{kind: clockInstant, seconds: float64(t.UnixNano()) / 1e9}
		}
	}
	return wallClock{}
}

// delta returns the seconds from a to b, and whether that is a quantity worth
// reporting at all.
//
// It declines in three cases, all of them "we do not know" rather than "zero":
// either reading failed to parse, the two are different kinds and subtracting
// them would be meaningless, or the result is negative.
//
// Negative is not clamped. A counter that resets (a game relaunching, a mod
// restarting its own clock) makes a later reading smaller than an earlier one,
// and the real elapsed time across that point was never recorded. Zero would
// assert continuity that never happened.
func delta(a, b wallClock) (int, bool) {
	if a.kind == clockNone || a.kind != b.kind {
		return 0, false
	}
	d := b.seconds - a.seconds
	if d < 0 {
		return 0, false
	}
	return int(d), true
}
