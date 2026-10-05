package main

import (
	"testing"
	"time"
)

func TestUploadBackoffWalksScheduleAndCaps(t *testing.T) {
	var b uploadBackoff
	want := []time.Duration{
		30 * time.Second,
		1 * time.Minute,
		5 * time.Minute,
		15 * time.Minute,
		30 * time.Minute,
		30 * time.Minute, // capped: stays at the final value
		30 * time.Minute,
	}
	for i, w := range want {
		if got := b.next(); got != w {
			t.Errorf("call %d: got %s, want %s", i, got, w)
		}
	}
}

func TestUploadBackoffResetReturnsToStart(t *testing.T) {
	var b uploadBackoff
	b.next() // 30s -> idx 1
	b.next() // 1m  -> idx 2
	b.reset()
	if got := b.next(); got != 30*time.Second {
		t.Errorf("after reset got %s, want 30s", got)
	}
}

func TestQueuedMessage(t *testing.T) {
	cases := []struct {
		n    int
		d    time.Duration
		want string
	}{
		{1, 30 * time.Second, "1 session queued — retrying in 30s"},
		{2, 5 * time.Minute, "2 sessions queued — retrying in 5m"},
		{3, 30 * time.Minute, "3 sessions queued — retrying in 30m"},
	}
	for _, c := range cases {
		if got := queuedMessage(c.n, c.d); got != c.want {
			t.Errorf("queuedMessage(%d, %s) = %q, want %q", c.n, c.d, got, c.want)
		}
	}
}

func TestCheckBackoffDoublesToCap(t *testing.T) {
	b := checkBackoff{rand: func() float64 { return 0.5 }} // 0.5 = no jitter
	want := []time.Duration{
		1 * time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		16 * time.Minute,
		32 * time.Minute,
		1 * time.Hour, // capped
		1 * time.Hour,
	}
	for i, w := range want {
		if got := b.next(); got != w {
			t.Errorf("failure %d: got %s, want %s", i+1, got, w)
		}
	}
}

func TestCheckBackoffJitterBounds(t *testing.T) {
	for _, tc := range []struct {
		r    float64
		want time.Duration
	}{
		{0, 48 * time.Second},          // -20%
		{0.999999, 72*time.Second - 1}, // just under +20%
	} {
		b := checkBackoff{rand: func() float64 { return tc.r }}
		got := b.next()
		if d := got - tc.want; d < -time.Millisecond || d > time.Millisecond {
			t.Errorf("r=%v: got %s, want ~%s", tc.r, got, tc.want)
		}
	}

	// The real source stays inside the band, including at the cap.
	var b checkBackoff
	for i := 0; i < 50; i++ {
		if got := b.next(); got < 48*time.Second || got > 72*time.Minute {
			t.Fatalf("failure %d: %s outside jitter band", i+1, got)
		}
	}
}

func TestCheckBackoffResetReturnsToBase(t *testing.T) {
	b := checkBackoff{rand: func() float64 { return 0.5 }}
	b.next()
	b.next()
	b.reset()
	if got := b.next(); got != checkRetryBase {
		t.Fatalf("after reset: got %s, want %s", got, checkRetryBase)
	}
}
