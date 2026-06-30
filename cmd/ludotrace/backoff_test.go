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
