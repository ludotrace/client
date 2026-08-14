package session

import (
	"encoding/json"
	"strconv"
	"time"
)

// clockKind distinguishes the two things a mod may put in wall_time. They are
// not comparable to each other, so a delta is only ever taken between two
// readings of the same kind.
type clockKind int

const (
	clockNone clockKind = iota
	// clockAbsolute is a real-world UTC instant (RFC 3339), which the mod spec
	// asks for and Stardew emits.
	clockAbsolute
	// clockRelative is a bare count of seconds from an origin the mod chose —
	// Fallout 4 emits Utility.GetCurrentRealTime(), seconds since the game
	// launched. Only differences within one game run mean anything: the counter
	// restarts from zero on relaunch, so a negative delta marks a restart, not
	// time running backwards.
	clockRelative
)

// wallClock is one parsed wall_time reading.
type wallClock struct {
	kind    clockKind
	seconds float64
}

// parseWallTime reads an event's wall_time field. It takes json.RawMessage
// rather than a string on purpose: the field is quoted in both shipping mods,
// but decoding it into a string would make a mod that emits a bare number fail
// to unmarshal, and a failed unmarshal drops the whole event. Timing detail is
// never worth losing an event over.
//
// A reading that does not parse yields clockNone, which every caller treats as
// "no information" rather than as a zero.
func parseWallTime(raw json.RawMessage) wallClock {
	if len(raw) == 0 {
		return wallClock{}
	}

	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return wallClock{}
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return wallClock{kind: clockAbsolute, seconds: float64(t.UnixNano()) / 1e9}
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return wallClock{kind: clockRelative, seconds: n}
		}
		return wallClock{}
	}

	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return wallClock{kind: clockRelative, seconds: n}
	}
	return wallClock{}
}

// delta returns the seconds from a to b when that is a meaningful quantity:
// both readings parsed, they are the same kind, and b is not earlier than a.
//
// A negative result is reported as unmeasurable rather than clamped to zero.
// Across a game restart a relative counter resets, so "later" reads as smaller;
// the real elapsed time is genuinely not in the data, and zero would assert
// continuity that never happened.
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
