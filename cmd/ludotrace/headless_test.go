package main

import "testing"

func TestHasArg(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"nil args", nil, false},
		{"program name only", []string{"ludotrace"}, false},
		{"bare flag", []string{"ludotrace", "--headless"}, true},
		{"absent", []string{"ludotrace", "--autostart"}, false},
		// A unit that starts the daemon on login passes both markers; --headless
		// is positional precisely so it need not come first.
		{"after another marker", []string{"ludotrace", "--autostart", "--headless"}, true},
		{"before another marker", []string{"ludotrace", "--headless", "--autostart"}, true},
		// argv[0] is never a flag, so a binary that happens to be named for one
		// must not turn the mode on.
		{"program name matches flag", []string{"--headless"}, false},
		{"prefix only is not a match", []string{"ludotrace", "--headless-mode"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasArg(tt.args, headlessArg); got != tt.want {
				t.Errorf("hasArg(%v, %q) = %v, want %v", tt.args, headlessArg, got, tt.want)
			}
		})
	}
}
