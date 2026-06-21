package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ludotrace/client/internal/auth"
	"github.com/ludotrace/client/internal/config"
	"github.com/ludotrace/client/internal/keychain"
	"github.com/ludotrace/client/internal/queue"
	"github.com/ludotrace/client/internal/session"
	"github.com/ludotrace/client/internal/tray"
	"github.com/ludotrace/client/internal/uploader"
	"github.com/ludotrace/client/internal/watcher"
)

var version = "dev"

func main() {
	level := slog.LevelInfo
	if strings.ToLower(os.Getenv("LUDOTRACE_LOG_LEVEL")) == "debug" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

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

	t := tray.New(authClient, q, cfg.CoreURL, cfg.AppURL, version)
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

	// Bridge signal cancellation → systray shutdown.
	go func() {
		<-ctx.Done()
		t.Quit()
	}()

	slog.Info("ludotrace client started", "core_url", cfg.CoreURL, "app_url", cfg.AppURL, "games", len(cfg.Games))
	t.Run() // blocks main goroutine until Quit() or systray exit
	stop()
}

func runUploadWorker(ctx context.Context, cfg *config.Config, authClient auth.Client, q *queue.Queue, extractors map[string]*session.Extractor, t *tray.Tray) {
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
			t.SetState(tray.StateIdle)
			continue
		}

		if errors.Is(err, uploader.ErrUnauthorized) {
			t.SetState(tray.StateNotAuth)
			// Trigger a refresh so the next iteration has a fresh token.
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

		// ErrTransient or unknown.
		slog.Warn("upload failed transiently", "game_id", item.GameID, "err", err)
		_ = q.UpdateAttempts(item.TmpPath)
		t.SetError(err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}
