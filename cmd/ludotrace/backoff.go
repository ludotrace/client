package main

import (
	"fmt"
	"time"
)

// backoffSchedule is the stepped retry delay for transient upload failures
// (network down, Core unreachable, 5xx). It escalates so a long outage is not
// hammered every 30 s, and caps at 30 min. Reset to the start on any success
// or on an explicit "Retry Now".
var backoffSchedule = []time.Duration{
	30 * time.Second,
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	30 * time.Minute,
}

// uploadBackoff tracks the current position in backoffSchedule. The zero value
// is ready to use and starts at the first (shortest) delay.
type uploadBackoff struct {
	idx int
}

// next returns the current delay and advances toward the cap. Repeated calls
// walk the schedule and then stay pinned at the final (capped) value.
func (b *uploadBackoff) next() time.Duration {
	d := backoffSchedule[b.idx]
	if b.idx < len(backoffSchedule)-1 {
		b.idx++
	}
	return d
}

// reset returns the backoff to the start of the schedule.
func (b *uploadBackoff) reset() {
	b.idx = 0
}

// queuedMessage renders the tray status line for the queued/offline state,
// e.g. "2 sessions queued — retrying in 5m".
func queuedMessage(n int, d time.Duration) string {
	noun := "sessions"
	if n == 1 {
		noun = "session"
	}
	return fmt.Sprintf("%d %s queued — retrying in %s", n, noun, humanizeDuration(d))
}

// humanizeDuration formats a backoff delay compactly: "30s", "1m", "15m".
func humanizeDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}
