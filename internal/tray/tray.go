package tray

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/ludotrace/client/internal/auth"
	"github.com/ludotrace/client/internal/browser"
	"github.com/ludotrace/client/internal/queue"
)

type State int

const (
	StateIdle         State = iota // authenticated, nothing uploading
	StateUploading                 // upload in flight
	StateError                     // last upload attempt failed permanently
	StateLimitReached              // 429 from Core
	StateNotAuth                   // not signed in
	StateNoGames                   // authenticated but no games configured
	StateQueued                    // transient failure: sessions queued, retrying with backoff
)

// String names the state for logs. Headless runs have no icon or status line,
// so this is the only rendering of state they get.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateUploading:
		return "uploading"
	case StateError:
		return "error"
	case StateLimitReached:
		return "limit_reached"
	case StateNotAuth:
		return "not_signed_in"
	case StateNoGames:
		return "no_games"
	case StateQueued:
		return "queued"
	default:
		return fmt.Sprintf("state(%d)", int(s))
	}
}

type Tray struct {
	auth          auth.Client
	q             *queue.Queue
	coreURL       string
	appURL        string
	version       string
	hasGames      bool
	stateCh       chan State
	errorMsg      string
	signInErr     string
	queuedMsg     string
	uploadingGame string
	droppedCount  int
	retryCh       chan struct{}
	signedInCh    chan struct{}

	updateCh          chan updateNote
	pendingRestartFn  func(pendingPath string)
	pendingUpdatePath string

	addGameFn       func()
	addGameInFlight atomic.Bool
}

type updateNote struct {
	version     string
	pendingPath string
	restartFn   func(pendingPath string) // called when "Restart to Update" is clicked
}

// SetHasGames records whether any games are configured, so the sign-in handler
// can pick the right post-auth state (Idle vs NoGames).
func (t *Tray) SetHasGames(v bool) {
	t.hasGames = v
}

func New(a auth.Client, q *queue.Queue, coreURL, appURL, version string) *Tray {
	return &Tray{
		auth:       a,
		q:          q,
		coreURL:    coreURL,
		appURL:     appURL,
		version:    version,
		stateCh:    make(chan State, 8),
		updateCh:   make(chan updateNote, 1),
		retryCh:    make(chan struct{}, 1),
		signedInCh: make(chan struct{}, 1),
	}
}

// SetAddGameHandler injects the callback invoked when the user clicks
// "Add Game" (either menu variant). The handler runs discovery, the file
// picker, config write, and watcher registration — composed in main.go, not
// here, since this package must stay ignorant of steam/config/watcher
// wiring. It is invoked on its own goroutine so the (potentially blocking,
// native-dialog-driven) handler never stalls the tray's click loop; the
// handler surfaces its own outcome via a native modal dialog (see main.go),
// so the tray does not carry any Add Game result state.
func (t *Tray) SetAddGameHandler(fn func()) {
	t.addGameFn = fn
}

// NotifyUpdateReady surfaces the "Restart to Update" tray item.
// restartFn is called when the user clicks that item; it should
// exec the pending binary and exit the current process.
func (t *Tray) NotifyUpdateReady(newVersion, pendingPath string, restartFn func(pendingPath string)) {
	// Non-blocking: if a note is already queued the new one replaces it
	// (draining first). Only one pending update exists at a time.
	select {
	case <-t.updateCh:
	default:
	}
	t.updateCh <- updateNote{version: newVersion, pendingPath: pendingPath, restartFn: restartFn}
}

// RunHeadless is Run's counterpart for a host with no display and no
// StatusNotifier host — SteamOS Gaming Mode above all, but equally a systemd
// user service or a container. It blocks until ctx is cancelled, so main can
// call it in Run's place without reshaping startup.
//
// It exists because systray cannot degrade: getlantern/systray's Linux backend
// is cgo GTK3, and gtk_init calls exit(1) when it cannot open a display, so
// Run does not fail — it takes the process down with it, before the watcher or
// the upload worker have done anything. Under a unit with Restart=on-failure
// that is a crash loop, not an outage you can read.
//
// Draining the channels is not incidental: stateCh is buffered 8, and with no
// event loop consuming it the ninth SetState would block the upload worker in
// place. Everything it drains is logged rather than drawn, because on a
// headless host the log file is the only signal the daemon has.
//
// Nothing here can be clicked, so the interactive menu items have no
// counterpart: Sign In, Add Game and Retry Now are unreachable, and a daemon
// that is signed out or unconfigured parks until someone fixes it from a
// desktop session. logState marks exactly those two states as warnings so the
// reason is in the log rather than inferred from silence.
func (t *Tray) RunHeadless(ctx context.Context) {
	slog.Info("tray disabled (headless) — state changes are logged, menu actions unavailable")
	for {
		select {
		case <-ctx.Done():
			return

		case s := <-t.stateCh:
			t.logState(s)

		case note := <-t.updateCh:
			// No one can click "Restart to Update" here, so say what actually
			// applies it. The staged binary is already on disk; the swap runs
			// on the next start of the service.
			slog.Info("update staged — restart the service to apply",
				"version", note.version, "pending_path", note.pendingPath)
		}
	}
}

// logState renders one state change as a log line, mirroring what applyState
// would have put on the status line.
//
// It reads the message fields (errorMsg, queuedMsg, …) the same way applyState
// does — published by the sender's SetState channel send — so it inherits that
// same contract rather than introducing a new one.
func (t *Tray) logState(s State) {
	attrs := []any{"state", s.String()}

	switch s {
	case StateUploading:
		slog.Info("client state", append(attrs, "game_id", t.uploadingGame)...)

	case StateError:
		slog.Error("client state", append(attrs, "detail", t.errorMsg)...)

	case StateQueued:
		slog.Info("client state", append(attrs, "detail", t.queuedMsg)...)

	case StateLimitReached:
		slog.Warn("client state", append(attrs, "detail", limitReachedTitle(t.droppedCount))...)

	case StateNotAuth:
		// Actionable, and only from a desktop session: there is no Sign In to
		// click here, so uploads stay parked until someone signs in with a
		// tray available and the token lands in the OS keychain.
		if t.signInErr != "" {
			attrs = append(attrs, "detail", t.signInErr)
		}
		slog.Warn("client state — sign in from a desktop session to resume uploads", attrs...)

	case StateNoGames:
		// Same shape: Add Game is a tray action, so a headless daemon with no
		// games configured needs config.toml edited by hand.
		slog.Warn("client state — no games configured; add one to config.toml", attrs...)

	default:
		slog.Info("client state", attrs...)
	}
}

// SetState pushes a new state to the tray. Safe for any goroutine.
func (t *Tray) SetState(s State) {
	t.stateCh <- s
}

// SetError sets the error message and transitions to StateError.
func (t *Tray) SetError(msg string) {
	t.errorMsg = msg
	t.SetState(StateError)
}

// SetSignInFailed surfaces a failed or timed-out sign-in attempt and returns
// the tray to StateNotAuth (Sign In / Quit only), with the reason on the
// status line. Distinct from SetError: StateError renders the full
// authenticated menu, which assumes a signed-in user — wrong for a sign-in
// that never succeeded. Sign In is only reachable from StateNotAuth, so a
// failure always returns there.
func (t *Tray) SetSignInFailed(msg string) {
	t.signInErr = msg
	t.SetState(StateNotAuth)
}

// SetQueued sets the queued status line (e.g. "2 sessions queued — retrying in
// 5m") and transitions to StateQueued. Used for transient/offline failures,
// which are recoverable and should not alarm the user like StateError.
func (t *Tray) SetQueued(msg string) {
	t.queuedMsg = msg
	t.SetState(StateQueued)
}

// RetryCh returns a channel that receives a signal when the user clicks
// "Retry Now". The upload worker selects on it to break its backoff sleep and
// retry immediately. Sends are non-blocking, so signals are coalesced.
func (t *Tray) RetryCh() <-chan struct{} {
	return t.retryCh
}

// SignedInCh returns a channel that receives a signal whenever the Sign In
// handler produces a usable session. The upload worker parks on it while
// signed out rather than polling GetToken on a timer: sign-in is a discrete
// user action this same process handles, so there is nothing to poll for.
// Sends are non-blocking, so signals are coalesced.
func (t *Tray) SignedInCh() <-chan struct{} {
	return t.signedInCh
}

// doSignIn runs the sign-in flow and applies its outcome to the tray. Called
// on its own goroutine from the Sign In click handler, since SignIn blocks on
// the browser round-trip.
func (t *Tray) doSignIn() {
	// Clear any reason left from a prior failed attempt so it can't leak
	// into a later StateNotAuth (e.g. after Sign Out).
	t.signInErr = ""
	err := t.auth.SignIn(context.Background())
	switch {
	case err == nil:
		// Fully signed in and persisted.
	case errors.Is(err, auth.ErrSignedInDegraded):
		// Signed in and durable, but the OS keychain is broken.
		// Stay signed in; just log — no need to alarm the user.
		slog.Warn("sign-in succeeded via encrypted fallback; OS keychain unavailable", "err", err)
	case errors.Is(err, auth.ErrSignedInNotPersisted):
		// Signed in for this session only. Surface it so the user knows
		// they'll have to sign in again after a restart. Still a usable
		// session, so the worker is woken all the same before returning early.
		t.signalSignedIn()
		t.SetError("Signed in, but couldn't save credentials — you may need to sign in again after restart (Windows Credential Manager may be full).")
		return
	default:
		// Sign-in itself failed (error, timeout, or ctx cancellation). No
		// session exists, so return to StateNotAuth with the reason — never
		// the authenticated menu that StateError would render.
		t.SetSignInFailed(err.Error())
		return
	}
	t.signalSignedIn()
	if t.hasGames {
		t.SetState(StateIdle)
	} else {
		t.SetState(StateNoGames)
	}
}

// signalSignedIn wakes a worker parked on SignedInCh. Non-blocking: if a
// signal is already pending the worker has not consumed it yet, and one wake
// is enough.
func (t *Tray) signalSignedIn() {
	select {
	case t.signedInCh <- struct{}{}:
	default:
	}
}

// NotifyDropped surfaces that count session(s) were dropped from the local
// queue for exceeding the max age — which only happens to a player who has
// been over quota too long, so the correct state is StateLimitReached with the
// drop count appended to the status line. The count accumulates across drop
// events within one over-quota spell and is cleared by ResetDropped on the
// next successful upload. Called only from the upload worker goroutine
// (alongside ResetDropped), so droppedCount has a single writer; the tray's
// event loop only reads it, published via the state-channel send SetState does.
func (t *Tray) NotifyDropped(count int) {
	if count <= 0 {
		return
	}
	t.droppedCount += count
	t.SetState(StateLimitReached)
}

// ResetDropped clears the accumulated drop count once an upload succeeds, so a
// later plain limit-reached (429) does not re-show a stale drop notice. Must be
// called from the same goroutine as NotifyDropped (the upload worker).
func (t *Tray) ResetDropped() {
	t.droppedCount = 0
}

// limitReachedTitle renders the StateLimitReached status line, appending the
// drop count when sessions have been aged out of the queue.
func limitReachedTitle(dropped int) string {
	if dropped <= 0 {
		return "Free upload limit reached"
	}
	noun := "sessions"
	if dropped == 1 {
		noun = "session"
	}
	return fmt.Sprintf("Free upload limit reached — %d old %s dropped", dropped, noun)
}

// SetUploading sets the game name displayed during upload and transitions to StateUploading.
func (t *Tray) SetUploading(gameName string) {
	t.uploadingGame = gameName
	t.SetState(StateUploading)
}

// triggerAddGame runs the injected Add Game handler on its own goroutine so
// a blocking native file-dialog call never stalls eventLoop (and, by
// extension, every other menu item).
//
// addGameInFlight guards against a double-click (or one click on each of
// the two "Add Game" menu variants) spawning two concurrent handler runs —
// without this, the second call would block silently behind the handler's
// own gamesMu, stuck behind a native dialog with no feedback that the click
// registered at all.
func (t *Tray) triggerAddGame() {
	if t.addGameFn == nil {
		slog.Warn("add game clicked but no handler registered")
		return
	}
	if !t.addGameInFlight.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer t.addGameInFlight.Store(false)
		t.addGameFn()
	}()
}

func openBrowser(target string) error {
	return browser.Open(target)
}
