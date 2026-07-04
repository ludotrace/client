package splash

import "sync"

// Handle is a live splash. It is always non-nil once returned by Show, even
// when no window was actually created (a non-interactive launch, an
// unsupported platform, or a creation failure) — in those cases Wait returns
// immediately. This keeps the caller branch-free: it can always call Wait.
type Handle struct {
	once sync.Once
	done chan struct{} // closed when the splash window has fully closed
}

// noopHandle returns an already-finished handle: Wait never blocks. Used for
// suppressed launches, non-Windows builds, and creation failures.
func noopHandle() *Handle {
	h := &Handle{done: make(chan struct{})}
	close(h.done)
	return h
}

// finish closes the handle's done channel exactly once, unblocking Wait.
func (h *Handle) finish() {
	h.once.Do(func() { close(h.done) })
}

// Wait blocks until the splash window has fully closed and released its
// resources. On a suppressed launch or a build/platform without a splash it
// returns immediately.
//
// The normal startup path does not call Wait — the splash auto-dismisses
// concurrently while the tray comes up. Wait exists for the singleton-lock
// contention path, where main is about to exit and must let the flash finish
// so a repeat double-click still produces visible feedback instead of exiting
// silently.
func (h *Handle) Wait() {
	if h == nil {
		return
	}
	<-h.done
}

// Show flashes the heartbeat splash if the launch is interactive, and returns a
// Handle. It never blocks: the window (Windows only) runs on its own goroutine
// and auto-dismisses after one 2x heartbeat cycle (~2.1s). A non-interactive
// launch, an unsupported platform, or a window-creation failure all yield an
// already-finished Handle — Show never returns nil and never fails the caller.
func Show(l Launch) *Handle {
	if !l.Interactive {
		return noopHandle()
	}
	return show()
}
