//go:build !headless

// The system tray, and everything that reaches getlantern/systray.
//
// It lives behind a build tag because that dependency is cgo GTK3 on Linux and
// links libgtk-3 and libayatana-appindicator3 into the binary whether or not a
// tray is ever drawn. SteamOS has no libayatana-appindicator3, so the default
// build cannot even start there — see docs/steam-deck.md. Building with
// `-tags headless` drops this file, and with it the GTK link.
//
// Nothing here may be referenced from tray.go: that file must stay compilable
// with the tag on.

package tray

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/getlantern/systray"
	"github.com/ludotrace/client/internal/config"
)

//go:embed assets/icon.ico
var iconIdle []byte

//go:embed assets/icon_error.ico
var iconError []byte

//go:embed assets/icon_uploading.ico
var iconUploading []byte

// Run starts the systray event loop. Blocks until the tray is quit.
// Must be called from the main goroutine on some platforms.
func (t *Tray) Run() {
	systray.Run(t.onReady, t.onExit)
}

// Quit signals the systray loop to exit, causing Run to return.
//
// Only valid after Run. A headless daemon has no systray loop to stop —
// systray.Quit() would reach into a GTK loop that was never started — and
// RunHeadless returns on its own ctx instead.
func (t *Tray) Quit() {
	systray.Quit()
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
		m.statusLine.SetTitle(limitReachedTitle(t.droppedCount))
		showAuthenticatedBase(m, false)

	case StateQueued:
		// Recoverable, not broken — keep the normal mark, not the error icon.
		systray.SetIcon(iconIdle)
		m.statusLine.SetTitle(t.queuedMsg)
		showAuthenticatedBase(m, false)
		m.retryNow.Show()

	case StateNotAuth:
		if t.signInErr != "" {
			systray.SetIcon(iconError)
			m.statusLine.SetTitle(fmt.Sprintf("Sign-in failed: %s", t.signInErr))
			m.statusLine.Show()
		}
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
			go t.doSignIn()

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
