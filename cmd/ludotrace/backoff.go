package main

import (
	"fmt"
	"math/rand/v2"
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

// Update-check retry bounds. A failed check retries after checkRetryBase,
// doubling up to checkRetryCap. The cap keeps a client that is offline for days
// (a Steam Deck off Wi-Fi is the normal case, not an outage) to about one
// attempt an hour, while a check that failed at wake from sleep — before the
// network is back — is retried within minutes instead of a full interval
// later (client#97).
const (
	checkRetryBase = 1 * time.Minute
	checkRetryCap  = 1 * time.Hour
	// checkRetryJitter spreads retries by ±20% so clients that failed together
	// (a shared outage, a fleet waking at the same hour) don't retry in step.
	checkRetryJitter = 0.2
)

// checkBackoff is the retry delay for failed update checks: exponential from
// checkRetryBase to checkRetryCap, with jitter. The zero value is ready to use.
type checkBackoff struct {
	failures int
	// rand returns a value in [0, 1); nil uses math/rand/v2. Tests pin it.
	rand func() float64
}

// next records a failure and returns the delay before the next attempt.
func (b *checkBackoff) next() time.Duration {
	d := checkRetryBase
	for i := 0; i < b.failures && d < checkRetryCap; i++ {
		d *= 2
	}
	d = min(d, checkRetryCap)
	b.failures++

	r := rand.Float64
	if b.rand != nil {
		r = b.rand
	}
	// Scale by a factor in [1-jitter, 1+jitter).
	return time.Duration(float64(d) * (1 - checkRetryJitter + 2*checkRetryJitter*r()))
}

// reset clears the failure count after a successful check.
func (b *checkBackoff) reset() { b.failures = 0 }

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
