package main

// Windows exe icon + version info, embedded via a generated .syso that "go
// build" picks up automatically when GOOS=windows. Regenerate with
// `go generate ./cmd/ludotrace/` (the Makefile does this before every
// build-windows). Not committed — see .gitignore — so the version info
// always reflects the current git tag rather than going stale.
//go:generate go tool go-winres simply --arch amd64 --out rsrc --icon ../../internal/tray/assets/app.ico --manifest gui --product-name LudoTrace --file-description "LudoTrace client daemon" --product-version git-tag --file-version git-tag

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sqweek/dialog"

	"github.com/ludotrace/client/internal/auth"
	"github.com/ludotrace/client/internal/autostart"
	"github.com/ludotrace/client/internal/config"
	"github.com/ludotrace/client/internal/keychain"
	"github.com/ludotrace/client/internal/knowngames"
	"github.com/ludotrace/client/internal/lock"
	"github.com/ludotrace/client/internal/proc"
	"github.com/ludotrace/client/internal/queue"
	"github.com/ludotrace/client/internal/session"
	"github.com/ludotrace/client/internal/splash"
	"github.com/ludotrace/client/internal/steam"
	"github.com/ludotrace/client/internal/tray"
	"github.com/ludotrace/client/internal/updater"
	"github.com/ludotrace/client/internal/uploader"
	"github.com/ludotrace/client/internal/version"
	"github.com/ludotrace/client/internal/watcher"
)

func main() {
	// --finish-update <original-path> [--parent-pid <pid>] [args...]
	// Run by the pending binary after the user clicks "Restart to Update".
	// Copies itself over the original path, deletes itself, relaunches.
	if len(os.Args) >= 3 && os.Args[1] == "--finish-update" {
		finishUpdate(os.Args[2], os.Args[3:])
		return
	}

	cfgDir, err := setupLogging()
	if err != nil {
		slog.Error("failed to prepare config dir", "err", err)
		os.Exit(1)
	}

	// --check-update: one-shot. Runs the same Check+Stage path as the
	// background worker, logs the outcome, and exits. Does not take the
	// singleton lock or start the tray, so it can run alongside a live
	// instance. Intended for development testing and ops diagnostics.
	if len(os.Args) >= 2 && os.Args[1] == "--check-update" {
		os.Exit(runCheckUpdate())
	}

	// Flash the launch splash on an interactive (double-click) launch. A
	// -H=windowsgui build gives zero feedback on double-click (#51), so this is
	// the only "it started" signal a user gets. It is fired here — before the
	// singleton lock — deliberately: a repeat double-click that will exit early
	// on ErrHeld must still flash, so the user isn't left thinking nothing
	// happened. Non-interactive launches (--autostart on login, --finish-update
	// / --check-update one-shots, or any other args) are suppressed by
	// DecideLaunch. Show is non-blocking and never fails the caller.
	splashHandle := splash.Show(splash.DecideLaunch(os.Args))

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	lockPath, err := config.LockPath()
	if err != nil {
		slog.Error("failed to resolve lock path", "err", err)
		os.Exit(1)
	}
	instanceLock, err := lock.Acquire(lockPath)
	if err != nil {
		slog.Error("another instance is already running", "lock", lockPath, "err", err)
		// Let the launch splash finish before exiting so a repeat double-click
		// still produces visible feedback instead of silently vanishing. Wait
		// returns immediately on a suppressed/non-Windows launch.
		splashHandle.Wait()
		os.Exit(1)
	}
	defer instanceLock.Release()

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

	// extractors is written by the startup loop below and, after startup, by
	// the Add Game handler (on its own goroutine); it is read concurrently
	// by the watcher callback and the upload worker. extractorStore guards
	// all of that with a mutex — a plain map here would race once Add Game
	// can add entries into an already-running process.
	extractors := newExtractorStore()
	for _, g := range cfg.Games {
		eventsPath := filepath.Join(g.WatchPath, g.EventsFile)
		offsetPath, _ := config.OffsetPath(g.GameID)
		extractors.set(g.GameID, session.New(g.GameID, eventsPath, offsetPath, os.TempDir(), q))
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
		ext, ok := extractors.get(gameID)
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

	// gamesMu serializes Add Game handler runs (concurrent clicks) and
	// guards cfg.Games appends alongside extractors' own locking.
	var gamesMu sync.Mutex
	t.SetAddGameHandler(makeAddGameHandler(ctx, &gamesMu, cfg, q, w, extractors, t, authClient))

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

	flushForShutdown(extractors)
}

// setupLogging installs the default slog handler and returns the config dir.
//
// Logging is teed to ludotrace.log in the config dir as well as stderr: a
// GUI-subsystem Windows build (-H=windowsgui, see Makefile) has no console, so
// stderr goes nowhere on a user's machine and the file is the only post-hoc
// diagnostic. stderr is kept alongside it for console/dev runs.
//
// An error means the config dir could not be resolved or created; the stderr
// handler is installed regardless, so a caller that can carry on still logs.
func setupLogging() (string, error) {
	level := slog.LevelInfo
	if strings.ToLower(os.Getenv("LUDOTRACE_LOG_LEVEL")) == "debug" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfgDir, err := config.Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		return "", err
	}
	if f := openLogFile(cfgDir); f != nil {
		// Tee tolerantly, not via io.MultiWriter: a GUI-subsystem build
		// (-H=windowsgui) has an invalid os.Stderr, and io.MultiWriter aborts
		// on the first writer's error — which would leave the log file empty,
		// silently defeating the whole point. tolerantTee writes to every
		// writer regardless, so a dead stderr can't suppress the file.
		w := &tolerantTee{writers: []io.Writer{os.Stderr, f}}
		slog.SetDefault(slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})))
		// f is intentionally left open for the lifetime of the process.
	}
	return cfgDir, nil
}

// tolerantTee fans each log record out to every writer, ignoring individual
// write errors so one dead writer never suppresses the others. This exists
// specifically because os.Stderr is an invalid handle under a -H=windowsgui
// build: io.MultiWriter would abort the whole write on stderr's error and
// never reach the log file. Always reports success, which slog treats as a
// clean write.
type tolerantTee struct{ writers []io.Writer }

func (t *tolerantTee) Write(p []byte) (int, error) {
	for _, w := range t.writers {
		_, _ = w.Write(p)
	}
	return len(p), nil
}

// maxLogBytes caps ludotrace.log before a single-generation rotation, so the
// always-on daemon never grows it unbounded.
const maxLogBytes = 5 << 20 // 5 MiB

// openLogFile opens (creating, appending to) ludotrace.log in cfgDir, rotating
// a prior log aside to ludotrace.log.old once it exceeds maxLogBytes. Returns
// nil on failure, in which case the caller keeps logging to stderr only — a
// missing log file must never stop the daemon from starting.
func openLogFile(cfgDir string) *os.File {
	logPath := filepath.Join(cfgDir, "ludotrace.log")
	if fi, err := os.Stat(logPath); err == nil && fi.Size() > maxLogBytes {
		// Best-effort rotation; on Windows a locked .old just means we keep
		// appending to the existing file, which is acceptable.
		_ = os.Rename(logPath, logPath+".old")
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("could not open log file; logging to stderr only", "path", logPath, "err", err)
		return nil
	}
	return f
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
		// The PID goes across so the child can wait for this process to be
		// fully gone before touching the install path. On Windows the
		// executable stays locked for a moment after os.Exit, and a rename
		// attempted inside that window fails with a sharing violation.
		args := []string{"--finish-update", self, parentPIDFlag, strconv.Itoa(os.Getpid())}
		args = append(args, os.Args[1:]...)
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

// parentPIDFlag carries the outgoing process's PID to the --finish-update
// child. It is optional: a pending binary is launched by the *previous*
// version, so a client updating from a release that predates the flag passes
// only the old positional form and the child falls back to retrying alone.
const parentPIDFlag = "--parent-pid"

const (
	// parentExitTimeout bounds the wait for the outgoing process to go away.
	// It is a lock-release wait, not a shutdown wait — the parent has already
	// called os.Exit by the time the child starts.
	parentExitTimeout = 15 * time.Second

	// renameAttempts and renameBackoff govern the retry around the swap
	// itself, which covers the parentless case and any handle the OS is slow
	// to release after the process is gone.
	renameAttempts = 8
	renameBackoff  = 250 * time.Millisecond
)

// finishUpdate is run by the newly-downloaded binary via --finish-update.
// It copies itself over originalPath, deletes the pending binary, then
// relaunches from the proper install location.
func finishUpdate(originalPath string, args []string) {
	// Logging comes first. This is the one path that can leave a user with no
	// client running at all, so it is the last one that should be silent —
	// and every failure below predates the normal startup logging setup.
	if _, err := setupLogging(); err != nil {
		slog.Error("finish-update: prepare log file", "err", err)
	}

	parentPID, remainingArgs := parseParentPID(args)

	self, err := os.Executable()
	if err != nil {
		slog.Error("finish-update: resolve self", "err", err)
		os.Exit(1)
	}

	os.Exit(runFinishUpdate(swapDeps{
		self:          self,
		originalPath:  originalPath,
		remainingArgs: remainingArgs,
		parentPID:     parentPID,
	}))
}

// parseParentPID splits an optional leading "--parent-pid <n>" off the
// argument list, returning the PID (0 if absent or unparseable) and the
// remaining args, which are the original launch flags to relaunch with.
func parseParentPID(args []string) (int, []string) {
	if len(args) >= 2 && args[0] == parentPIDFlag {
		pid, err := strconv.Atoi(args[1])
		if err != nil {
			return 0, args[2:]
		}
		return pid, args[2:]
	}
	return 0, args
}

// swapDeps holds the inputs to the update swap plus the seams the tests use to
// drive its failure branches. A nil function field takes the real behaviour.
type swapDeps struct {
	self          string
	originalPath  string
	remainingArgs []string
	parentPID     int

	waitForExit func(pid int, timeout time.Duration) bool
	rename      func(oldpath, newpath string) error
	launch      func(path string, args []string) error
	sleep       func(time.Duration)
}

// runFinishUpdate performs the swap and returns a process exit code.
//
// Its governing rule is that it must never return with nothing running: if the
// new binary cannot be put in place, the old one — still intact on disk — is
// relaunched and the failure reported through a non-zero code. A stale version
// running beats no version running, because the client is the only thing that
// will ever offer the user another update.
func runFinishUpdate(d swapDeps) int {
	waitForExit := d.waitForExit
	if waitForExit == nil {
		waitForExit = proc.WaitForExit
	}
	rename := d.rename
	if rename == nil {
		rename = os.Rename
	}
	launch := d.launch
	if launch == nil {
		launch = startDetached
	}
	sleep := d.sleep
	if sleep == nil {
		sleep = time.Sleep
	}

	// Wait for the process holding originalPath to be gone before trying to
	// replace it. Missing or still-live is not fatal — the retry below covers
	// both — so this only ever logs.
	if d.parentPID > 0 && !waitForExit(d.parentPID, parentExitTimeout) {
		slog.Warn("finish-update: parent still running; attempting swap anyway",
			"parent_pid", d.parentPID, "waited", parentExitTimeout)
	}

	tmpPath, err := stageBinary(d.self, d.originalPath)
	if err != nil {
		slog.Error("finish-update: stage new binary", "err", err)
		return relaunchOriginal(d, launch, 1)
	}

	if err := renameWithRetry(rename, sleep, tmpPath, d.originalPath); err != nil {
		os.Remove(tmpPath)
		slog.Error("finish-update: rename", "attempts", renameAttempts, "err", err)
		return relaunchOriginal(d, launch, 1)
	}

	// Best-effort remove of the pending binary (self). This succeeds on
	// Unix (a running binary can be unlinked) but fails on Windows, where a
	// running .exe is locked. The leftover is reconciled on next startup by
	// the relaunched install-path process via updater.ClearStaged — by then
	// this process has exited and the file is unlocked. The sidecar is also
	// cleared there, so leaving it in place here is intentional.
	_ = os.Remove(d.self)

	if err := launch(d.originalPath, d.remainingArgs); err != nil {
		slog.Error("finish-update: relaunch", "path", d.originalPath, "err", err)
		return 1
	}
	slog.Info("finish-update: swap complete", "path", d.originalPath)
	return 0
}

// stageBinary copies the running binary to a temp file alongside originalPath
// — the same directory, so the swap is a rename within one filesystem rather
// than a copy that could be seen half-written — and returns its path.
func stageBinary(self, originalPath string) (string, error) {
	src, err := os.Open(self)
	if err != nil {
		return "", fmt.Errorf("open self: %w", err)
	}
	defer src.Close()

	tmp, err := os.CreateTemp(filepath.Dir(originalPath), "ludotrace-install-*")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("copy binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("chmod: %w", err)
	}
	return tmpPath, nil
}

// renameWithRetry retries the swap on a fixed backoff. The failure it exists
// for is a Windows sharing violation on an executable whose process has only
// just exited, which clears on its own within moments; there is no way to ask
// the OS when the handle is released, so it is retried rather than waited on.
func renameWithRetry(rename func(string, string) error, sleep func(time.Duration), tmpPath, originalPath string) error {
	var err error
	for attempt := 1; attempt <= renameAttempts; attempt++ {
		if err = rename(tmpPath, originalPath); err == nil {
			return nil
		}
		if attempt < renameAttempts {
			slog.Warn("finish-update: rename failed, retrying",
				"attempt", attempt, "of", renameAttempts, "err", err)
			sleep(renameBackoff)
		}
	}
	return err
}

// relaunchOriginal starts the untouched old binary so a failed swap does not
// leave the machine with no client. Returns failCode, or 1 if even the
// relaunch fails — there is nothing further to try at that point.
func relaunchOriginal(d swapDeps, launch func(string, []string) error, failCode int) int {
	if err := launch(d.originalPath, d.remainingArgs); err != nil {
		slog.Error("finish-update: could not relaunch original after failed swap; no client is running",
			"path", d.originalPath, "err", err)
		return 1
	}
	slog.Warn("finish-update: swap failed, relaunched previous version",
		"path", d.originalPath, "staged_version", version.Version)
	return failCode
}

// startDetached launches path and returns without waiting for it.
func startDetached(path string, args []string) error {
	cmd := exec.Command(path, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Start()
}

func runUploadWorker(ctx context.Context, cfg *config.Config, authClient auth.Client, q *queue.Queue, extractors *extractorStore, t *tray.Tray) {
	var backoff uploadBackoff
	retryCh := t.RetryCh()

	// A single reusable timer for all backoff/retry delays in this loop,
	// rather than a fresh time.After per branch — under rapid repeated
	// errors those would otherwise accumulate in the runtime timer heap
	// until each one fires.
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	// wait blocks for d, or until ctx is cancelled. Returns false if the
	// caller should return (ctx cancelled).
	wait := func(d time.Duration) bool {
		timer.Reset(d)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return false
		case <-timer.C:
			return true
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Age-based drop-oldest: evict any session that has sat in the queue
		// past the configured max age (oldest-first) before selecting, and
		// clean up its temp file — keeping the tail bounded for a chronically
		// over-quota player. Derived from persisted CapturedAt, so it holds
		// across a restart. This bounds the tail; selection below serves the
		// head newest-first — the two orders are intentionally distinct.
		maxAge := time.Duration(cfg.QueueMaxAgeDays) * 24 * time.Hour
		if dropped, derr := q.EvictExpired(maxAge, time.Now()); derr != nil {
			slog.Warn("failed to evict expired queue items", "err", derr)
		} else if len(dropped) > 0 {
			for _, it := range dropped {
				if rerr := os.Remove(it.TmpPath); rerr != nil && !os.IsNotExist(rerr) {
					slog.Warn("failed to remove dropped temp file", "path", it.TmpPath, "err", rerr)
				}
			}
			slog.Warn("dropped old sessions, over quota too long",
				"count", len(dropped), "max_age_days", cfg.QueueMaxAgeDays)
			t.NotifyDropped(len(dropped))
		}

		if q.Len() == 0 {
			if !wait(5 * time.Second) {
				return
			}
			continue
		}

		// Newest-first: attempt the most recently captured session, not the
		// oldest arrival. The over-quota player's scarce quota budget should go
		// to the session they just played, not an ever-staler backlog.
		item, ok := q.PeekNewest()
		if !ok {
			continue
		}

		token, err := authClient.GetToken(ctx)
		if err != nil {
			if errors.Is(err, auth.ErrNotSignedIn) || errors.Is(err, auth.ErrTokenRevoked) {
				t.SetState(tray.StateNotAuth)
				if !wait(10 * time.Second) {
					return
				}
				continue
			}
			slog.Warn("failed to get auth token", "err", err)
			if !wait(5 * time.Second) {
				return
			}
			continue
		}

		t.SetUploading(item.GameID)

		jobID, traceID, err := uploader.Upload(ctx, cfg.CoreURL, item.GameID, item.TmpPath, token, item.Capture)
		if err == nil {
			retireItem(q, extractors, item)
			slog.Info("upload succeeded", "game_id", item.GameID, "job_id", jobID)
			backoff.reset()
			t.ResetDropped()
			t.SetState(tray.StateIdle)
			continue
		}

		if errors.Is(err, uploader.ErrUnauthorized) {
			t.SetState(tray.StateNotAuth)
			_, _ = authClient.GetToken(ctx)
			if !wait(2 * time.Second) {
				return
			}
			continue
		}

		if errors.Is(err, uploader.ErrLimitReached) {
			t.SetState(tray.StateLimitReached)
			_ = q.UpdateAttempts(item.TmpPath)
			if !wait(limitReachedWait(err)) {
				return
			}
			continue
		}

		var badReq uploader.ErrBadRequest
		if errors.Is(err, uploader.ErrFileTooLarge) || errors.As(err, &badReq) {
			slog.Error("upload failed permanently, dropping item", "game_id", item.GameID, "err", err, "trace_id", traceID)
			// Retire the item exactly as a success does, minus the upload:
			// Core will never accept these bytes, so the read position has to
			// move past them. Leaving the offset put means Extract() rebuilds
			// the same rejected session on the next write event, forever.
			retireItem(q, extractors, item)
			// client#64: trace_id in the tray text is the only place a
			// support case can pick it up without digging through the log.
			t.SetError(fmt.Sprintf("%s (trace %s)", err.Error(), traceID))
			continue
		}

		// ErrTransient or unknown — recoverable. Queue the sessions and retry
		// with escalating backoff rather than alarming the user with an error.
		slog.Warn("upload failed transiently", "game_id", item.GameID, "err", err, "trace_id", traceID)
		_ = q.UpdateAttempts(item.TmpPath)
		delay := backoff.next()
		t.SetQueued(queuedMessage(q.Len(), delay))
		timer.Reset(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		case <-retryCh:
			if !timer.Stop() {
				<-timer.C
			}
			slog.Info("retry requested — resetting backoff")
			backoff.reset()
		}
	}
}

// retireItem takes an item out of the pipeline for good: advance the game's
// read position past it, delete its temp file, and drop it from the queue.
// Both terminal outcomes use this — a 202 and a permanent rejection differ only
// in whether Core kept the bytes, not in what the client still owes them.
//
// Removal is by TmpPath, never positional: the worker selects with PeekNewest,
// so popping the head would retire a different, innocent item and leave this
// one in the queue.
func retireItem(q *queue.Queue, extractors *extractorStore, item queue.Item) {
	if ext, ok := extractors.get(item.GameID); ok {
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
}

const (
	// limitReachedDefaultWait is the poll interval used on a 429 when Core did
	// not send a usable Retry-After hint (older Core, or the header absent).
	limitReachedDefaultWait = 60 * time.Second
	// limitReachedMinWait floors an honored Retry-After. Core's real hint is on
	// the order of days, but an aggressively small value (e.g. "Retry-After: 1")
	// would otherwise have us hammer Core's advisory-locked count query for no
	// benefit; 5s is the tightest re-poll we'll honor.
	limitReachedMinWait = 5 * time.Second
	// limitReachedMaxWait caps an honored Retry-After. Free-tier quota windows
	// are ~3 days, but a bogus or huge header value (or clock skew on an
	// HTTP-date form) must not park the worker effectively forever; a 24h
	// ceiling bounds the wait so we re-check at least daily regardless.
	limitReachedMaxWait = 24 * time.Hour
)

// limitReachedWait decides how long to wait after a 429 upload_limit_reached.
// It honors a server-provided Retry-After hint (carried on
// *uploader.LimitReachedError) clamped to [limitReachedMinWait,
// limitReachedMaxWait], and falls back to limitReachedDefaultWait when no valid
// hint is present.
func limitReachedWait(err error) time.Duration {
	var lre *uploader.LimitReachedError
	if errors.As(err, &lre) && lre.RetryAfter > 0 {
		d := lre.RetryAfter
		if d < limitReachedMinWait {
			d = limitReachedMinWait
		}
		if d > limitReachedMaxWait {
			d = limitReachedMaxWait
		}
		return d
	}
	return limitReachedDefaultWait
}

// extractorStore is a mutex-guarded map[string]*session.Extractor. Before
// Add Game, this map was only ever written once at startup (before any
// other goroutine existed) and read concurrently thereafter by the watcher
// callback and the upload worker — safe without locking. Add Game now
// writes into it from a live goroutine after startup, so a plain map would
// race; this wrapper is the minimal fix.
type extractorStore struct {
	mu   sync.RWMutex
	byID map[string]*session.Extractor
}

func newExtractorStore() *extractorStore {
	return &extractorStore{byID: make(map[string]*session.Extractor)}
}

func (s *extractorStore) get(gameID string) (*session.Extractor, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byID[gameID]
	return e, ok
}

func (s *extractorStore) set(gameID string, e *session.Extractor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[gameID] = e
}

// all returns a snapshot of the extractors, so a caller can iterate without
// holding the lock across work that takes one of its own.
func (s *extractorStore) all() map[string]*session.Extractor {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*session.Extractor, len(s.byID))
	for id, e := range s.byID {
		out[id] = e
	}
	return out
}

// flushForShutdown enqueues whatever each game has open as the Client exits,
// rather than leaving it for an indefinite next launch. Extraction, not upload:
// the worker is already stopping, so these land in the durable queue and go out
// on the next run.
//
// Best-effort by design — a game that fails to flush is logged and the exit
// continues. Its bytes are still on disk with the offset unmoved, so the next
// launch re-reads them.
func flushForShutdown(extractors *extractorStore) {
	for gameID, ext := range extractors.all() {
		if err := ext.FlushForShutdown(); err != nil {
			slog.Error("shutdown flush failed", "game_id", gameID, "err", err)
		}
	}
}

// makeAddGameHandler composes the Add Game flow: Steam auto-discovery,
// falling to the folder-picker (recognized-folder resolution or a
// known-games prompt for an unrecognized one) when nothing new was found,
// then writing the result to config.toml and registering it with the live
// watcher/extractor — all without a restart.
//
// gamesMu serializes concurrent invocations (e.g. a double-click on "Add
// Game") so cfg.Games appends and extractor registration can't race each
// other.
//
// w.AddGame is called before config.AppendGame (the reverse of a strictly
// literal reading of the spec's task list) so that a path which vanishes
// between pick and add — the one case the whole flow has to guard against —
// fails before anything is written to config.toml. Writing the config
// entry first and having w.AddGame fail afterwards would leave a zombie
// config.toml row that the dedupe guard (steam.DedupeNew / the picker's
// "already added" check) would treat as already-configured, permanently
// blocking any future retry short of hand-editing the file.
//
// The extractor is registered before w.AddGame makes the watch live, not
// after: a WRITE event landing in the window between "watch goes live" and
// "extractor registered" would hit cb in main.go, find no extractor, and be
// silently dropped. Registering the extractor first closes that window;
// watcher.AddGame's own idempotency guard makes it safe even if AddGame
// then fails and the whole handler is retried for the same game.
func makeAddGameHandler(ctx context.Context, gamesMu *sync.Mutex, cfg *config.Config, q *queue.Queue, w *watcher.Watcher, extractors *extractorStore, t *tray.Tray, authClient auth.Client) func() {
	return func() {
		gamesMu.Lock()
		defer gamesMu.Unlock()

		g, err := resolveNewGame(ctx, cfg, authClient)
		if err != nil {
			var aa *alreadyAddedError
			if errors.As(err, &aa) {
				notifyGameInfo(aa.Error())
			} else {
				notifyGameError(err)
			}
			return
		}
		if g == nil {
			// User cancelled the picker, or cancelled the unrecognized-folder
			// prompt without picking a known game. Not an error: no
			// confirmation, no failure message, tray stays as-is.
			return
		}

		eventsPath := filepath.Join(g.WatchPath, g.EventsFile)
		offsetPath, err := config.OffsetPath(g.GameID)
		if err != nil {
			slog.Error("add game: failed to resolve offset path", "game_id", g.GameID, "err", err)
			notifyGameError(fmt.Errorf("could not resolve offset path for %q: %w", g.GameID, err))
			return
		}
		extractors.set(g.GameID, session.New(g.GameID, eventsPath, offsetPath, os.TempDir(), q))

		if err := w.AddGame(*g); err != nil {
			slog.Warn("add game: watcher registration failed", "game_id", g.GameID, "err", err)
			notifyGameError(fmt.Errorf("could not watch %q: %w", g.WatchPath, err))
			return
		}

		if err := config.AppendGame(*g); err != nil {
			slog.Error("add game: failed to write config", "game_id", g.GameID, "err", err)
			notifyGameError(err)
			return
		}
		cfg.Games = append(cfg.Games, *g)

		t.SetHasGames(true)
		t.SetState(tray.StateIdle)

		slog.Info("game added", "game_id", g.GameID, "watch_path", g.WatchPath)
		notifyGameAdded(displayName(g.GameID))
	}
}

// alreadyAddedError signals a benign "nothing to do" outcome: the picked
// folder resolved to a game_id already present in cfg.Games. Not a failure
// — the caller surfaces it via notifyGameInfo, not notifyGameError's error
// framing.
type alreadyAddedError struct {
	displayName string
}

func (e *alreadyAddedError) Error() string {
	return fmt.Sprintf("%s is already added", e.displayName)
}

// notifyGameAdded/notifyGameInfo/notifyGameError surface the outcome of an
// Add Game action as a native modal dialog. The tray menu closes the instant
// its item is clicked, so a menu-item title is never seen in the moment — a
// modal is the only feedback a non-technical user reliably notices, and the
// picker already proves native dialogs work from this handler goroutine.
// These run on the (already off-tray-loop) handler goroutine and block until
// dismissed, which harmlessly keeps triggerAddGame's in-flight guard set so a
// second click can't start a parallel run behind the dialog.
func notifyGameAdded(gameName string) {
	dialog.Message("%s was added and is now being tracked.", gameName).
		Title("LudoTrace — Game Added").Info()
}

func notifyGameInfo(msg string) {
	dialog.Message("%s", msg).Title("LudoTrace — Add Game").Info()
}

func notifyGameError(err error) {
	dialog.Message("Add Game failed:\n\n%s", err).Title("LudoTrace — Add Game").Error()
}

// resolveNewGame runs the two-stage Add Game flow and returns the game to
// add. A nil game with a nil error means the user cancelled — the caller
// shows neither a confirmation nor a failure.
func resolveNewGame(ctx context.Context, cfg *config.Config, authClient auth.Client) (*config.Game, error) {
	discovered, err := steam.Discover()
	if err != nil {
		slog.Warn("steam discovery failed", "err", err)
	}
	if newGames := steam.DedupeNew(discovered, cfg.Games); len(newGames) > 0 {
		return &newGames[0], nil
	}

	picked, err := dialog.Directory().Title("Select Game Folder").SetStartDir(pickerStartDir()).Browse()
	if err != nil {
		if errors.Is(err, dialog.ErrCancelled) {
			return nil, nil
		}
		return nil, fmt.Errorf("folder picker: %w", err)
	}
	if picked == "" {
		// Belt-and-suspenders: every sqweek/dialog backend is expected to
		// return ErrCancelled on cancel (handled above), but an empty path
		// reaching filepath.Base("") -> "." would otherwise flow into
		// folder-matching as if "." had been picked. Treat it as a cancel.
		return nil, nil
	}

	folderName := filepath.Base(picked)
	if kg, ok := steam.MatchFolder(folderName); ok {
		g := steam.GameFromFolder(kg, picked)
		if gameConfigured(cfg.Games, g.GameID) {
			return nil, &alreadyAddedError{displayName: kg.DisplayName}
		}
		return &g, nil
	}

	// Unrecognized folder: the dropdown of known games comes from Core's
	// GET /v1/games (client#29), not a compile-time list, so a game added to
	// Core's registry appears here without a client release. sqweek/dialog
	// has no native list-selection widget, so each remaining candidate is
	// offered as a Yes/No prompt instead of a real dropdown (see the spec's
	// Design Notes for why). The picked folder itself becomes watch_path —
	// these games' watch paths aren't Steam-derivable, so there is no
	// per-game default-path table; the events-file name follows from game_id.
	knownGames, err := knowngames.List(ctx, cfg.CoreURL, authClient)
	if err != nil {
		return nil, fmt.Errorf("could not load known games list: %w", err)
	}
	offered := false
	for _, kg := range knownGames {
		if gameConfigured(cfg.Games, kg.GameID) {
			continue
		}
		offered = true
		prompt := fmt.Sprintf("The folder %q wasn't recognized automatically.\n\nAdd it as %q?", folderName, kg.Name)
		if dialog.Message("%s", prompt).Title("Unrecognized Folder").YesNo() {
			g := steam.GameFromID(kg.GameID, picked)
			return &g, nil
		}
	}
	if !offered {
		// Every known game is already configured — there was nothing left
		// to offer. Without this, the loop above silently returns (nil,
		// nil) and the user sees no feedback at all after picking a folder.
		return nil, &alreadyAddedError{displayName: "every supported game"}
	}
	return nil, nil
}

// pickerStartDir defaults to Steam's common library folder when detected
// (Windows only), else the user's home directory. See the I/O matrix's
// "Registry read fails" row and the non-Windows acceptance criterion.
func pickerStartDir() string {
	if dir, ok := steam.DefaultBrowseDir(); ok {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

func gameConfigured(games []config.Game, gameID string) bool {
	for _, g := range games {
		if g.GameID == gameID {
			return true
		}
	}
	return false
}

// displayName looks up a display name for gameID: first the local Steam
// registry (auto-discovery matches), then the knowngames cache — warmed by
// resolveNewGame's List call moments earlier in the same handler run, so a
// cache-only (no network) read is enough here. Falls back to the game_id
// itself if neither has it.
func displayName(gameID string) string {
	for _, kg := range steam.Registry {
		if kg.GameID == gameID {
			return kg.DisplayName
		}
	}
	if games, err := knowngames.ReadCache(); err == nil {
		for _, g := range games {
			if g.GameID == gameID {
				return g.Name
			}
		}
	}
	return gameID
}
