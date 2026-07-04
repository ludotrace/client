package tray

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/getlantern/systray"
	"github.com/ludotrace/client/internal/auth"
	"github.com/ludotrace/client/internal/browser"
	"github.com/ludotrace/client/internal/config"
	"github.com/ludotrace/client/internal/queue"
)

//go:embed assets/icon.ico
var iconIdle []byte

//go:embed assets/icon_error.ico
var iconError []byte

//go:embed assets/icon_uploading.ico
var iconUploading []byte

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

type Tray struct {
	auth          auth.Client
	q             *queue.Queue
	coreURL       string
	appURL        string
	version       string
	hasGames      bool
	stateCh       chan State
	errorMsg      string
	queuedMsg     string
	uploadingGame string
	retryCh       chan struct{}

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
		auth:     a,
		q:        q,
		coreURL:  coreURL,
		appURL:   appURL,
		version:  version,
		stateCh:  make(chan State, 8),
		updateCh: make(chan updateNote, 1),
		retryCh:  make(chan struct{}, 1),
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

// Run starts the systray event loop. Blocks until the tray is quit.
// Must be called from the main goroutine on some platforms.
func (t *Tray) Run() {
	systray.Run(t.onReady, t.onExit)
}

// Quit signals the systray loop to exit, causing Run to return.
func (t *Tray) Quit() {
	systray.Quit()
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

// SetUploading sets the game name displayed during upload and transitions to StateUploading.
func (t *Tray) SetUploading(gameName string) {
	t.uploadingGame = gameName
	t.SetState(StateUploading)
}

// menuItems holds every possible menu item pre-created so we can show/hide
// them rather than rebuild the menu (systray does not support dynamic rebuilding).
// Separators are not included because AddSeparator returns void and cannot be toggled.
type menuItems struct {
	// Authenticated states (Idle / Uploading / Error / LimitReached / NoGames)
	statusLine  *systray.MenuItem
	addGame     *systray.MenuItem
	manageGames *systray.MenuItem
	openDash    *systray.MenuItem
	retry       *systray.MenuItem
	retryNow    *systray.MenuItem
	signOut     *systray.MenuItem
	quit        *systray.MenuItem

	// StateNotAuth
	signIn *systray.MenuItem
	quitNA *systray.MenuItem

	// StateNoGames overrides
	addGameNG  *systray.MenuItem
	openDashNG *systray.MenuItem
	signOutNG  *systray.MenuItem
	quitNG     *systray.MenuItem

	// Always visible — not included in hideAll.
	versionItem *systray.MenuItem

	// Update notification — shown when a staged update is waiting.
	updateLabel   *systray.MenuItem
	restartUpdate *systray.MenuItem
}

func buildMenu() *menuItems {
	m := &menuItems{}

	// All items pre-created; shown/hidden per state.
	// Separators are intentionally omitted — AddSeparator returns void on
	// Windows and cannot be hidden, so persistent separators bleed across states.

	m.statusLine = systray.AddMenuItem("LudoTrace — Idle", "")
	m.statusLine.Disable()
	m.addGame = systray.AddMenuItem("Add Game", "")
	m.manageGames = systray.AddMenuItem("Manage Games", "")
	m.openDash = systray.AddMenuItem("Open Dashboard", "")
	m.retry = systray.AddMenuItem("Retry", "")
	m.retryNow = systray.AddMenuItem("Retry Now", "")
	m.signOut = systray.AddMenuItem("Sign Out", "")
	m.quit = systray.AddMenuItem("Quit", "")

	m.signIn = systray.AddMenuItem("Sign In", "")
	m.quitNA = systray.AddMenuItem("Quit", "")

	m.addGameNG = systray.AddMenuItem("Add Game", "")
	m.openDashNG = systray.AddMenuItem("Open Dashboard", "")
	m.signOutNG = systray.AddMenuItem("Sign Out", "")
	m.quitNG = systray.AddMenuItem("Quit", "")

	m.versionItem = systray.AddMenuItem("", "")
	m.versionItem.Disable()

	// Update items — hidden until a staged update is ready.
	m.updateLabel = systray.AddMenuItem("", "")
	m.updateLabel.Disable()
	m.restartUpdate = systray.AddMenuItem("Restart to Update", "")
	m.updateLabel.Hide()
	m.restartUpdate.Hide()

	return m
}

func (t *Tray) onReady() {
	systray.SetTitle("LudoTrace")
	systray.SetTooltip(fmt.Sprintf("LudoTrace %s", t.version))
	systray.SetIcon(iconIdle)

	m := buildMenu()
	m.versionItem.SetTitle(fmt.Sprintf("version %s", t.version))

	// Render initial state before any SetState call arrives.
	t.applyState(StateIdle, m)

	go t.eventLoop(m)
}

func (t *Tray) onExit() {}

func (t *Tray) applyState(s State, m *menuItems) {
	hideAll(m)

	switch s {
	case StateIdle:
		systray.SetIcon(iconIdle)
		m.statusLine.SetTitle("LudoTrace — Idle")
		showAuthenticatedBase(m, false)

	case StateUploading:
		systray.SetIcon(iconUploading)
		m.statusLine.SetTitle(fmt.Sprintf("Uploading: %s…", t.uploadingGame))
		showAuthenticatedBase(m, false)

	case StateError:
		systray.SetIcon(iconError)
		m.statusLine.SetTitle(fmt.Sprintf("Error: %s", t.errorMsg))
		showAuthenticatedBase(m, true)

	case StateLimitReached:
		systray.SetIcon(iconError)
		m.statusLine.SetTitle("Free upload limit reached")
		showAuthenticatedBase(m, false)

	case StateQueued:
		// Recoverable, not broken — keep the normal mark, not the error icon.
		systray.SetIcon(iconIdle)
		m.statusLine.SetTitle(t.queuedMsg)
		showAuthenticatedBase(m, false)
		m.retryNow.Show()

	case StateNotAuth:
		m.signIn.Show()
		m.quitNA.Show()

	case StateNoGames:
		systray.SetIcon(iconIdle)
		m.addGameNG.Show()
		m.openDashNG.Show()
		m.signOutNG.Show()
		m.quitNG.Show()
	}
}

func showAuthenticatedBase(m *menuItems, showRetry bool) {
	m.statusLine.Show()
	m.addGame.Show()
	m.manageGames.Show()
	m.openDash.Show()
	if showRetry {
		m.retry.Show()
	}
	m.signOut.Show()
	m.quit.Show()
}

func hideAll(m *menuItems) {
	m.statusLine.Hide()
	m.addGame.Hide()
	m.manageGames.Hide()
	m.openDash.Hide()
	m.retry.Hide()
	m.retryNow.Hide()
	m.signOut.Hide()
	m.quit.Hide()

	m.signIn.Hide()
	m.quitNA.Hide()

	m.addGameNG.Hide()
	m.openDashNG.Hide()
	m.signOutNG.Hide()
	m.quitNG.Hide()
}

// eventLoop is the tray's single event goroutine: state changes, update
// notifications, and menu clicks all funnel through one select rather than
// three separate always-blocked goroutines — cuts idle goroutine count
// without changing behavior, since each source was already independent and
// non-blocking apart from its own handler.
func (t *Tray) eventLoop(m *menuItems) {
	for {
		select {
		case s := <-t.stateCh:
			t.applyState(s, m)

		case note := <-t.updateCh:
			m.updateLabel.SetTitle(fmt.Sprintf("Update %s ready", note.version))
			m.updateLabel.Show()
			m.restartUpdate.Show()
			// Store the restart function so the click case below can invoke it.
			t.pendingRestartFn = note.restartFn
			t.pendingUpdatePath = note.pendingPath

		case <-m.signIn.ClickedCh:
			go func() {
				err := t.auth.SignIn(context.Background())
				switch {
				case err == nil:
					// Fully signed in and persisted.
				case errors.Is(err, auth.ErrSignedInDegraded):
					// Signed in and durable, but the OS keychain is broken.
					// Stay signed in; just log — no need to alarm the user.
					slog.Warn("sign-in succeeded via encrypted fallback; OS keychain unavailable", "err", err)
				case errors.Is(err, auth.ErrSignedInNotPersisted):
					// Signed in for this session only. Surface it so the user
					// knows they'll have to sign in again after a restart.
					t.SetError("Signed in, but couldn't save credentials — you may need to sign in again after restart (Windows Credential Manager may be full).")
					return
				default:
					// Sign-in itself failed.
					t.SetError(err.Error())
					return
				}
				if t.hasGames {
					t.SetState(StateIdle)
				} else {
					t.SetState(StateNoGames)
				}
			}()

		case <-m.signOut.ClickedCh:
			go func() {
				_ = t.auth.SignOut(context.Background())
				t.SetState(StateNotAuth)
			}()

		case <-m.signOutNG.ClickedCh:
			go func() {
				_ = t.auth.SignOut(context.Background())
				t.SetState(StateNotAuth)
			}()

		case <-m.addGame.ClickedCh:
			t.triggerAddGame()

		case <-m.addGameNG.ClickedCh:
			t.triggerAddGame()

		case <-m.manageGames.ClickedCh:
			go func() {
				path, err := config.FilePath()
				if err != nil {
					t.SetError(err.Error())
					return
				}
				if err := openBrowser(path); err != nil {
					t.SetError(err.Error())
				}
			}()

		case <-m.openDash.ClickedCh:
			go func() {
				if err := openBrowser(t.appURL); err != nil {
					t.SetError(err.Error())
				}
			}()

		case <-m.openDashNG.ClickedCh:
			go func() {
				if err := openBrowser(t.appURL); err != nil {
					t.SetError(err.Error())
				}
			}()

		case <-m.restartUpdate.ClickedCh:
			if t.pendingRestartFn != nil {
				t.pendingRestartFn(t.pendingUpdatePath)
			}

		case <-m.retry.ClickedCh:
			t.SetState(StateIdle)

		case <-m.retryNow.ClickedCh:
			// Non-blocking: if a signal is already pending, the worker hasn't
			// consumed it yet — one retry is enough.
			select {
			case t.retryCh <- struct{}{}:
			default:
			}

		case <-m.quit.ClickedCh:
			systray.Quit()

		case <-m.quitNA.ClickedCh:
			systray.Quit()

		case <-m.quitNG.ClickedCh:
			systray.Quit()
		}
	}
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
