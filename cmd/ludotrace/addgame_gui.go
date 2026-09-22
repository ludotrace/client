//go:build !headless

// The Add Game flow's user interaction: the folder picker and the modal
// result dialogs, via github.com/sqweek/dialog.
//
// Tagged out of the headless build for the same reason as the tray — dialog's
// Linux backend is cgo GTK3, so importing it links libgtk-3 into the binary
// even on a daemon that never opens a window. See internal/tray/tray_gui.go.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/sqweek/dialog"

	"github.com/ludotrace/client/internal/auth"
	"github.com/ludotrace/client/internal/config"
	"github.com/ludotrace/client/internal/knowngames"
	"github.com/ludotrace/client/internal/steam"
)

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
