// Package capture describes how the Client captured an uploaded region: what
// it can assert about the region's boundaries, as opposed to what is inside it.
//
// This is the Client's half of core#116. Core validates every value against a
// closed enum and re-marshals what it accepts, so a field this package gets
// wrong is rejected at upload rather than passed to the model — the enum
// constants here must stay in step with core/internal/capture.
//
// The Context exists to let the model say "the opening of this run is missing"
// instead of hedging. Absence of a duration is not a fact it can act on; an
// explicit opener: absent is.
package capture

import "encoding/json"

// Opener states whether the session_start that began this run is inside the
// uploaded region. A carried-forward upload leads in with bytes whose opener
// was flushed in an earlier upload, so it reports absent.
const (
	OpenerPresent = "present"
	OpenerAbsent  = "absent"
)

// ClosedBy values name what ended the captured region.
//
// The first three are boundaries the Client observes: a new session_start
// superseded the open one, the events file went quiet past the inactivity
// threshold, or the Client itself exited. SizeCap is not a boundary in the
// play at all — it is the Client cutting a carried-forward fragment that grew
// past its hold cap while still waiting for an opener, so that region is cut
// mid-play and the run continues past the end of the upload.
const (
	ClosedBySuperseded     = "superseded"
	ClosedByIdleTimeout    = "idle_timeout"
	ClosedByClientShutdown = "client_shutdown"
	ClosedBySizeCap        = "size_cap"
)

// Context is what accompanies one upload as its capture_context form field.
//
// GapBefore is a pointer because "no carried-forward fragment" and "a gap of
// zero seconds" are different facts, and only the former may be omitted. It is
// also omitted when the gap cannot be measured — see session.wallClock, where a
// game restart resets a relative wall_time counter and leaves no way to know how
// long the player was away.
type Context struct {
	Opener     string `json:"opener"`
	ClosedBy   string `json:"closed_by"`
	GapBefore  *int   `json:"gap_before,omitempty"`
	EventCount int    `json:"event_count"`
	SpanS      int    `json:"span_s"`
}

// Encode renders the Context as the JSON object Core expects. A nil Context
// yields no bytes: an upload that reports nothing is always valid, and Core
// leaves the block out entirely rather than inventing a not-reported state.
func Encode(c *Context) ([]byte, error) {
	if c == nil {
		return nil, nil
	}
	return json.Marshal(c)
}
