package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ludotrace/client/internal/auth"
	"github.com/ludotrace/client/internal/autostart"
	"github.com/ludotrace/client/internal/config"
	"github.com/ludotrace/client/internal/keychain"
	"github.com/ludotrace/client/internal/queue"
	"github.com/ludotrace/client/internal/session"
	"github.com/ludotrace/client/internal/tray"
	"github.com/ludotrace/client/internal/updater"
	"github.com/ludotrace/client/internal/uploader"
	"github.com/ludotrace/client/internal/version"
	"github.com/ludotrace/client/internal/watcher"
)

func main() {
	// --finish-update <original-path> [args...]
	// Run by the pending binary after the user clicks "Restart to Update".
	// Copies itself over the original path, deletes itself, relaunches.
	if len(os.Args) >= 3 && os.Args[1] == "--finish-update" {
		finishUpdate(os.Args[2], os.Args[3:])
		return
	}

	level := slog.LevelInfo
	if strings.ToLower(os.Getenv("LUDOTRACE_LOG_LEVEL")) == "debug" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// --check-update: one-shot. Runs the same Check+Stage path as the
	// background worker, logs the outcome, and exits. Does not take the
	// singleton lock or start the tray, so it can run alongside a live
	// instance. Intended for development testing and ops diagnostics.
	if len(os.Args) >= 2 && os.Args[1] == "--check-update" {
		os.Exit(runCheckUpdate())
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	cfgDir, err := config.Dir()
	if err != nil {
		slog.Error("failed to resolve config dir", "err", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		slog.Error("failed to create config dir", "err", err)
		os.Exit(1)
	}

	lockPath, err := config.LockPath()
	if err != nil {
		slog.Error("failed to resolve lock path", "err", err)
		os.Exit(1)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		slog.Error("another instance is already running", "lock", lockPath)
		os.Exit(1)
	}
	lockFile.Close()
	defer os.Remove(lockPath)

	autostart.Register()

	credPath, err := config.CredentialPath()
	if err != nil {
		slog.Error("failed to resolve credential path", "err", err)
		os.Exit(1)
	}
	kc := keychain.New(credPath)
	authClient := auth.New(auth.Config{CoreURL: cfg.CoreURL}, kc)

	queuePath, err := config.QueuePath()
	if err != nil {
		slog.Error("failed to resolve queue path", "err", err)
		os.Exit(1)
	}
	q, err := queue.New(queuePath)
	if err != nil {
		slog.Error("failed to init queue", "err", err)
		os.Exit(1)
	}

	extractors := make(map[string]*session.Extractor)
	for _, g := range cfg.Games {
		eventsPath := filepath.Join(g.WatchPath, g.EventsFile)
		offsetPath, _ := config.OffsetPath(g.GameID)
		extractors[g.GameID] = session.New(g.GameID, eventsPath, offsetPath, os.TempDir(), q)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	t := tray.New(authClient, q, cfg.CoreURL, cfg.AppURL, version.Version)
	t.SetHasGames(len(cfg.Games) > 0)

	if !authClient.IsSignedIn() {
		t.SetState(tray.StateNotAuth)
	} else if len(cfg.Games) == 0 {
		t.SetState(tray.StateNoGames)
	}

	go runUploadWorker(ctx, cfg, authClient, q, extractors, t)

	cb := func(gameID string) {
		ext, ok := extractors[gameID]
		if !ok {
			return
		}
		if err := ext.Extract(); err != nil {
			slog.Error("session extraction failed", "game_id", gameID, "err", err)
		}
	}

	w, err := watcher.New(cfg.Games, cb, 2*time.Second)
	if err != nil {
		slog.Error("failed to init watcher", "err", err)
		os.Exit(1)
	}
	go func() {
		if err := w.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("watcher stopped", "err", err)
		}
	}()

	// Reconcile any staged update left on disk. A staged binary newer than the
	// running version is a genuine pending update → re-surface the tray item.
	// One that is not newer is a stale leftover from an already-applied update
	// (the pending process could not delete itself on Windows) → clear it now
	// that we are the relaunched install-path process and the file is unlocked.
	if stagedVer, ok := updater.StagedVersion(cfgDir); ok {
		if updater.IsNewer(stagedVer, version.Version) {
			slog.Info("previously staged update found, surfacing tray item", "version", stagedVer)
			t.NotifyUpdateReady(stagedVer, updater.PendingPath(cfgDir), makeRestartFn())
		} else if err := updater.ClearStaged(cfgDir); err != nil {
			slog.Warn("failed to clear stale staged update", "err", err)
		} else {
			slog.Info("cleared stale staged update", "staged_version", stagedVer, "running_version", version.Version)
		}
	}

	go runUpdateWorker(ctx, cfgDir, t)

	// Bridge signal cancellation → systray shutdown.
	go func() {
		<-ctx.Done()
		t.Quit()
	}()

	slog.Info("ludotrace client started",
		"core_url", cfg.CoreURL,
		"app_url", cfg.AppURL,
		"games", len(cfg.Games),
		"version", version.Version,
	)
	t.Run() // blocks until Quit() or systray exit
	stop()
}

// runUpdateWorker checks for a newer version on startup then on a
// server-controlled interval (default 24 h).
func runUpdateWorker(ctx context.Context, cfgDir string, t *tray.Tray) {
	u := updater.New(cfgDir)
	interval := 4 * time.Hour

	doCheck := func() {
		upd, pendingPath, next := checkAndStage(ctx, u)
		if next > 0 {
			interval = next
		}
		if upd != nil && pendingPath != "" {
			t.NotifyUpdateReady(upd.Version, pendingPath, makeRestartFn())
		}
	}

	doCheck()

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			doCheck()
		}
	}
}

// checkAndStage runs one version check and, if a newer version is available,
// stages it. Returns the update, the staged binary path (empty if nothing was
// staged), and the interval until the next check. Shared by the background
// worker and the --check-update one-shot.
func checkAndStage(ctx context.Context, u *updater.Updater) (*updater.Update, string, time.Duration) {
	upd, next, err := u.Check(ctx)
	if err != nil {
		slog.Warn("version check failed", "err", err)
	}
	if upd == nil {
		return nil, "", next
	}
	pendingPath, err := u.Stage(ctx, upd)
	if err != nil {
		slog.Warn("failed to stage update", "version", upd.Version, "err", err)
		return upd, "", next
	}
	return upd, pendingPath, next
}

// runCheckUpdate is the --check-update one-shot. It performs a single
// check-and-stage cycle and reports the outcome. Returns a process exit code:
// 0 = up to date / dev build skipped / staged successfully, 1 = staging error.
func runCheckUpdate() int {
	cfgDir, err := config.Dir()
	if err != nil {
		slog.Error("failed to resolve config dir", "err", err)
		return 1
	}
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		slog.Error("failed to create config dir", "err", err)
		return 1
	}

	slog.Info("running one-shot update check", "version", version.Version)
	u := updater.New(cfgDir)
	upd, pendingPath, _ := checkAndStage(context.Background(), u)

	switch {
	case upd == nil:
		slog.Info("no update available", "current", version.Version)
		return 0
	case pendingPath == "":
		slog.Error("update found but staging failed", "version", upd.Version)
		return 1
	default:
		slog.Info("update staged and ready", "version", upd.Version, "pending_path", pendingPath)
		return 0
	}
}

// makeRestartFn returns the function passed to tray.NotifyUpdateReady.
// When the user clicks "Restart to Update" the pending binary is launched
// with --finish-update pointing at the current install path, then this
// process exits.
func makeRestartFn() func(pendingPath string) {
	return func(pendingPath string) {
		self, err := os.Executable()
		if err != nil {
			slog.Error("restart: could not resolve current executable", "err", err)
			return
		}
		args := append([]string{"--finish-update", self}, os.Args[1:]...)
		cmd := exec.Command(pendingPath, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			slog.Error("restart: failed to launch pending binary", "err", err)
			return
		}
		os.Exit(0)
	}
}

// finishUpdate is run by the newly-downloaded binary via --finish-update.
// It copies itself over originalPath, deletes the pending binary, then
// relaunches from the proper install location.
func finishUpdate(originalPath string, remainingArgs []string) {
	self, err := os.Executable()
	if err != nil {
		slog.Error("finish-update: resolve self", "err", err)
		os.Exit(1)
	}

	src, err := os.Open(self)
	if err != nil {
		slog.Error("finish-update: open self", "err", err)
		os.Exit(1)
	}

	dir := filepath.Dir(originalPath)
	tmp, err := os.CreateTemp(dir, "ludotrace-install-*")
	if err != nil {
		src.Close()
		slog.Error("finish-update: create temp", "err", err)
		os.Exit(1)
	}
	tmpPath := tmp.Name()

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		src.Close()
		os.Remove(tmpPath)
		slog.Error("finish-update: copy binary", "err", err)
		os.Exit(1)
	}
	tmp.Close()
	src.Close()

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		os.Remove(tmpPath)
		slog.Error("finish-update: chmod", "err", err)
		os.Exit(1)
	}

	if err := os.Rename(tmpPath, originalPath); err != nil {
		os.Remove(tmpPath)
		slog.Error("finish-update: rename", "err", err)
		os.Exit(1)
	}

	// Best-effort remove of the pending binary (self). This succeeds on
	// Unix (a running binary can be unlinked) but fails on Windows, where a
	// running .exe is locked. The leftover is reconciled on next startup by
	// the relaunched install-path process via updater.ClearStaged — by then
	// this process has exited and the file is unlocked. The sidecar is also
	// cleared there, so leaving it in place here is intentional.
	_ = os.Remove(self)

	cmd := exec.Command(originalPath, remainingArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		slog.Error("finish-update: relaunch", "err", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runUploadWorker(ctx context.Context, cfg *config.Config, authClient auth.Client, q *queue.Queue, extractors map[string]*session.Extractor, t *tray.Tray) {
	var backoff uploadBackoff
	retryCh := t.RetryCh()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if q.Len() == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		item, ok := q.Peek()
		if !ok {
			continue
		}

		token, err := authClient.GetToken(ctx)
		if err != nil {
			if errors.Is(err, auth.ErrNotSignedIn) || errors.Is(err, auth.ErrTokenRevoked) {
				t.SetState(tray.StateNotAuth)
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Second):
				}
				continue
			}
			slog.Warn("failed to get auth token", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		t.SetUploading(item.GameID)

		jobID, err := uploader.Upload(ctx, cfg.CoreURL, item.GameID, item.TmpPath, token)
		if err == nil {
			if ext, ok := extractors[item.GameID]; ok {
				if aerr := ext.AdvanceOffset(item.EndOffset); aerr != nil {
					slog.Error("failed to advance offset", "game_id", item.GameID, "err", aerr)
				}
			}
			if rerr := os.Remove(item.TmpPath); rerr != nil && !os.IsNotExist(rerr) {
				slog.Warn("failed to remove temp file", "path", item.TmpPath, "err", rerr)
			}
			if rerr := q.Remove(item.TmpPath); rerr != nil {
				slog.Warn("failed to remove queue item", "err", rerr)
			}
			slog.Info("upload succeeded", "game_id", item.GameID, "job_id", jobID)
			backoff.reset()
			t.SetState(tray.StateIdle)
			continue
		}

		if errors.Is(err, uploader.ErrUnauthorized) {
			t.SetState(tray.StateNotAuth)
			_, _ = authClient.GetToken(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		if errors.Is(err, uploader.ErrLimitReached) {
			t.SetState(tray.StateLimitReached)
			_ = q.UpdateAttempts(item.TmpPath)
			select {
			case <-ctx.Done():
				return
			case <-time.After(60 * time.Second):
			}
			continue
		}

		var badReq uploader.ErrBadRequest
		if errors.Is(err, uploader.ErrFileTooLarge) || errors.As(err, &badReq) {
			slog.Error("upload failed permanently, dropping item", "game_id", item.GameID, "err", err)
			q.Dequeue()
			t.SetError(err.Error())
			continue
		}

		// ErrTransient or unknown — recoverable. Queue the sessions and retry
		// with escalating backoff rather than alarming the user with an error.
		slog.Warn("upload failed transiently", "game_id", item.GameID, "err", err)
		_ = q.UpdateAttempts(item.TmpPath)
		delay := backoff.next()
		t.SetQueued(queuedMessage(q.Len(), delay))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		case <-retryCh:
			slog.Info("retry requested — resetting backoff")
			backoff.reset()
		}
	}
}
