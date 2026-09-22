//go:build headless

package tray

import "log/slog"

// Run is the tray entrypoint the non-headless build provides. This build has no
// tray at all, so it cannot honour the call.
//
// It is unreachable in practice: a headless build forces headless mode in main
// regardless of flags (see cmd/ludotrace/mode_headless.go), so main calls
// RunHeadless. Keeping the signature satisfied is what lets main.go stay free of
// build tags — and returning immediately, rather than blocking, means a future
// caller that does reach it fails visibly instead of hanging forever.
func (t *Tray) Run() {
	slog.Error("this build has no tray support; run with --headless")
}

// Quit matches Run: there is no systray loop to stop.
func (t *Tray) Quit() {}
