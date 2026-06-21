package tray

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"

	"github.com/getlantern/systray"
	"github.com/ludotrace/client/internal/auth"
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
	StateError                     // last upload attempt failed
	StateLimitReached              // 429 from Core
	StateNotAuth                   // not signed in
	StateNoGames                   // authenticated but no games configured
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
	uploadingGame string
}

// SetHasGames records whether any games are configured, so the sign-in handler
// can pick the right post-auth state (Idle vs NoGames).
func (t *Tray) SetHasGames(v bool) {
	t.hasGames = v
}

func New(a auth.Client, q *queue.Queue, coreURL, appURL, version string) *Tray {
	return &Tray{
		auth:    a,
		q:       q,
		coreURL: coreURL,
		appURL:  appURL,
		version: version,
		// Buffer so SetState never blocks a caller.
		stateCh: make(chan State, 8),
	}
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

	go t.stateLoop(m)
	go t.clickLoop(m)
}

func (t *Tray) onExit() {}

func (t *Tray) stateLoop(m *menuItems) {
	for s := range t.stateCh {
		t.applyState(s, m)
	}
}

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
	m.signOut.Hide()
	m.quit.Hide()

	m.signIn.Hide()
	m.quitNA.Hide()

	m.addGameNG.Hide()
	m.openDashNG.Hide()
	m.signOutNG.Hide()
	m.quitNG.Hide()
}

func (t *Tray) clickLoop(m *menuItems) {
	for {
		select {
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
			slog.Info("Add Game clicked — dialog not yet implemented")

		case <-m.addGameNG.ClickedCh:
			slog.Info("Add Game clicked — dialog not yet implemented")

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

		case <-m.retry.ClickedCh:
			t.SetState(StateIdle)

		case <-m.quit.ClickedCh:
			systray.Quit()

		case <-m.quitNA.ClickedCh:
			systray.Quit()

		case <-m.quitNG.ClickedCh:
			systray.Quit()
		}
	}
}

func openBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}
