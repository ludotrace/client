package splash

import "testing"

func TestDecideLaunch(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		interactive bool
	}{
		{"bare double-click", []string{"ludotrace.exe"}, true},
		{"bare, no args at all", []string{}, true},
		{"nil args", nil, true},
		{"autostart on login", []string{"ludotrace.exe", "--autostart"}, false},
		{"finish-update one-shot", []string{"ludotrace.exe", "--finish-update", "C:\\path\\ludotrace.exe"}, false},
		{"check-update one-shot", []string{"ludotrace.exe", "--check-update"}, false},
		{"unknown extra arg still suppressed", []string{"ludotrace.exe", "--whatever"}, false},
		{"multiple args suppressed", []string{"ludotrace.exe", "foo", "bar"}, false},
		{"autostart plus trailing arg", []string{"ludotrace.exe", "--autostart", "x"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DecideLaunch(tt.args).Interactive
			if got != tt.interactive {
				t.Fatalf("DecideLaunch(%v).Interactive = %v, want %v", tt.args, got, tt.interactive)
			}
		})
	}
}

func TestShowSuppressedIsNonBlocking(t *testing.T) {
	// A suppressed launch must return an already-finished handle: Show must not
	// open a window and Wait must return immediately. (An interactive launch is
	// not exercised here — it would try to open a window, which these tests
	// must never do; the Windows window path is covered by the cross-build.)
	h := Show(DecideLaunch([]string{"ludotrace.exe", "--autostart"}))
	if h == nil {
		t.Fatal("Show returned nil handle")
	}
	h.Wait() // must not block
}

func TestSuppressMarkers(t *testing.T) {
	// These specific markers must always be recognized as non-interactive.
	for _, m := range []string{"--autostart", "--finish-update", "--check-update"} {
		if !isSuppressMarker(m) {
			t.Errorf("expected %q to be a recognized suppress marker", m)
		}
	}
	if isSuppressMarker("--nope") {
		t.Error("did not expect --nope to be a suppress marker")
	}
}
