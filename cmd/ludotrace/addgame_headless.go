//go:build headless

package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/ludotrace/client/internal/auth"
	"github.com/ludotrace/client/internal/config"
)

// The headless build has no folder picker and no modal dialogs, so Add Game
// has no interactive path. These stand in for the GTK-backed versions in
// addgame_gui.go so main.go needs no build tags of its own.
//
// Reachable only via the tray's Add Game item, which this build also drops —
// so in practice nothing calls them. They log rather than panic so that a
// future non-tray caller degrades instead of taking the daemon down.

// errNoDesktop explains the one thing a headless operator can do about it.
var errNoDesktop = errors.New("Add Game needs a desktop session; add the game to config.toml instead")

func resolveNewGame(context.Context, *config.Config, auth.Client) (*config.Game, error) {
	return nil, errNoDesktop
}

func notifyGameAdded(gameName string) { slog.Info("game added", "game", gameName) }

func notifyGameInfo(msg string) { slog.Info("add game", "detail", msg) }

func notifyGameError(err error) { slog.Error("add game failed", "err", err) }
