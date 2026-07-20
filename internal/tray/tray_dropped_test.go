package tray

import "testing"

func TestLimitReachedTitle(t *testing.T) {
	cases := []struct {
		dropped int
		want    string
	}{
		{0, "Free upload limit reached"},
		{-1, "Free upload limit reached"},
		{1, "Free upload limit reached — 1 old session dropped"},
		{3, "Free upload limit reached — 3 old sessions dropped"},
	}
	for _, c := range cases {
		if got := limitReachedTitle(c.dropped); got != c.want {
			t.Errorf("limitReachedTitle(%d) = %q, want %q", c.dropped, got, c.want)
		}
	}
}

// drainStates non-blockingly reads every state currently queued on stateCh.
func drainStates(t *Tray) []State {
	var out []State
	for {
		select {
		case s := <-t.stateCh:
			out = append(out, s)
		default:
			return out
		}
	}
}

func TestNotifyDropped_AccumulatesAndSignalsLimitReached(t *testing.T) {
	tr := New(nil, nil, "", "", "")

	tr.NotifyDropped(2)
	tr.NotifyDropped(1)

	if tr.droppedCount != 3 {
		t.Errorf("droppedCount = %d, want 3 (accumulated across drop events)", tr.droppedCount)
	}
	states := drainStates(tr)
	if len(states) != 2 {
		t.Fatalf("queued %d state changes, want 2", len(states))
	}
	for i, s := range states {
		if s != StateLimitReached {
			t.Errorf("state[%d] = %v, want StateLimitReached", i, s)
		}
	}
}

func TestNotifyDropped_ZeroIsIgnored(t *testing.T) {
	tr := New(nil, nil, "", "", "")
	tr.NotifyDropped(0)
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount = %d, want 0", tr.droppedCount)
	}
	if got := drainStates(tr); len(got) != 0 {
		t.Errorf("NotifyDropped(0) pushed %d states, want 0", len(got))
	}
}

func TestResetDropped_ClearsCount(t *testing.T) {
	tr := New(nil, nil, "", "", "")
	tr.NotifyDropped(4)
	tr.ResetDropped()
	if tr.droppedCount != 0 {
		t.Errorf("droppedCount after reset = %d, want 0", tr.droppedCount)
	}
}
