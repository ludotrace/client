# CLAUDE.md — LudoTrace Client

## What this is

LudoTrace Client is a lightweight background daemon that watches a game's append-only live events file, extracts complete sessions from it, and uploads them to LudoTrace Core for processing.

It is intentionally dumb. No UI beyond a system tray icon. No LLM calls. Watch events file → extract sessions → authenticate → compress → upload → advance offset. Session boundary detection is the only logic it owns.

---

## Responsibilities

- Run as a singleton background daemon (one instance per OS session)
- Watch each game's append-only live events file for WRITE events (game-agnostic: any game with a config entry)
- Extract play sessions, bounded by `session_start` markers and a 30-min inactivity timeout (see Watcher Logic)
- Track read position per game via a sidecar offset file; advance offset only on 202 from Core
- Authenticate with LudoTrace Core via browser-based OAuth (Clerk)
- Store auth token securely in OS keychain
- Upload extracted sessions to Core API with correct game_id via a durable queue
- Delete temp session file and advance sidecar offset on 202 confirmation only
- System tray icon showing status (idle, uploading, error) with "Add Game" as the primary setup action
- Cross-platform: macOS, Windows, Linux

---

## Tech Stack

- **Language:** Go
- **File watching:** `fsnotify` (cross-platform file system events)
- **HTTP client:** Go standard library `net/http`
- **Auth:** Browser-based OAuth via Clerk, localhost callback
- **Token storage:** OS keychain via `zalando/go-keyring`
- **System tray:** `getlantern/systray`
- **Config:** TOML file in OS config directory

---

## Project Structure

```
client/
├── cmd/
│   └── ludotrace/    — main entrypoint; wires config, auth, watcher, queue, tray
├── internal/
│   ├── config/       — TOML load + validation, env overrides, Add Game writer
│   ├── auth/         — Clerk OAuth flow, token refresh, ErrNotAuthenticated
│   ├── keychain/     — token storage (zalando/go-keyring)
│   ├── watcher/      — fsnotify wrapper, WRITE-event watch, debounce, startup trigger
│   ├── session/      — session extraction, orphan detection, sidecar offset, temp file writer
│   ├── queue/        — durable upload queue (queue.json), Contains(), enqueue, dequeue
│   ├── uploader/     — gzip + multipart POST to Core, response handling, backoff, offset advancement
│   └── tray/         — systray icon + menu, state derivation, Add Game dialog
├── assets/
│   ├── icon.png      — tray icon (synthwave mark)
│   ├── icon_error.png
│   └── icon_uploading.png
├── Makefile
└── CLAUDE.md
```

---

## Config File

Stored at OS config directory (`~/.config/ludotrace/config.toml` on Linux/macOS, `%APPDATA%\ludotrace\config.toml` on Windows):

```toml
core_url = "https://core.ludotrace.com"

[[games]]
game_id    = "fallout4"
watch_path = "C:/Users/Username/Documents/My Games/Fallout4"
events_file = "lt_fo4_events.jsonl"

[[games]]
game_id    = "stardew"
watch_path = "C:/Users/Username/AppData/Roaming/StardewValley/Saves"
events_file = "lt_stardew_events.jsonl"
```

---

## Auth Flow

The Client never calls Clerk directly — Core proxies the entire OAuth flow. This is a
deliberate boundary (architecture spine AD-3): exactly two credential paths exist system-wide,
and this is the machine/headless one.

```
1. Client checks OS keychain for a valid opaque token
2. If no token or expired:
   a. Open system browser to Core's GET /auth/signin
   b. Start localhost HTTP server (loopback listener) on a random port
   c. Core handles the Clerk OAuth exchange, then redirects to the loopback
      listener with an opaque token (Core-issued, not a Clerk token)
   d. Store the opaque token in OS keychain
3. On each upload, exchange the opaque token for a short-lived Core-signed JWT
   via POST /v1/auth/token (HS256, 60s lifetime, issuer ludotrace-core)
4. Use the JWT as Authorization: Bearer on that request; repeat step 3 per upload
   rather than caching the JWT past its lifetime
```

```go
type AuthClient interface {
    GetToken(ctx context.Context) (string, error)
    Refresh(ctx context.Context) error
    Logout() error
}
```

---

## Watcher Logic

The mod writes a single append-only live events file (`lt_<game>_events.jsonl`). The Client watches this file for WRITE events and extracts complete sessions from it.

```go
// Pseudocode
for each game in config.Games {
    eventsPath := filepath.Join(game.WatchPath, game.EventsFile)
    watcher.Add(eventsPath)
}

on WRITE(eventsPath):
    debounce(2s, func() {
        session.ExtractAndEnqueue(game) // reads from sidecar offset, splits into play sessions on session_start / inactivity
    })
// Upload worker runs separately; tray state is derived from queue + auth state
```

Key behaviors:
- **WRITE events, not CREATE** — the events file is long-lived and append-only; watch for modifications.
- **Debounce** — coalesce burst WRITE events; wait for 2s quiet before extracting, to avoid reading partial lines.
- **Session extraction** — reads new lines from the sidecar offset and groups them into play sessions. The only structural boundary is `session_start`: a new one closes the previous session (the game was reloaded) and flushes it. `session_end` is **not** a boundary — it is opaque payload buffered into the current session. This is deliberate and game-agnostic: games emit `session_end` on different cadences (Fallout 4 writes one per *save*, so a single play session contains many), so treating it as a terminator would split or drop data based on a game-specific quirk. Each closed session is written to a temp file and enqueued.
- **Inactivity flush (orphan)** — the open session is flushed when its events file has gone >30 min without a write. This is the generic "the player stopped" signal and is what closes the final session of a play period — including one that ended with no clean `session_end` at all (crash, or quit with no final save).
- **Sidecar offset** — stored at `<config_dir>/offsets/<game_id>.offset`. Advanced only on 202. If missing, starts from byte 0 (full reprocess).
- **Startup extraction** — on startup, immediately trigger extraction for each configured game.
- **Missing events file** — watch the parent directory; promote to file watch on CREATE.
- **Durable queue** — extracted sessions go to `queue.json`; a separate worker goroutine dequeues and uploads. Watcher/extractor never calls upload directly.
- **Delete on confirm** — delete temp session file only after Core returns 202.

---

## Upload

```go
// POST /v1/upload
func Upload(gameID string, filePath string, token string) error {
    // multipart form upload
    // game_id field + session_file field
    // Authorization: Bearer <token>
    // expect 202 Accepted
}
```

---

## System Tray States

The daemon is game-agnostic and a singleton. The tray is the primary management surface.

| State | Icon | Menu |
|-------|------|------|
| Idle, authenticated | Normal mark | Add Game, Manage Games, Open Dashboard, Sign Out, Quit |
| Uploading | Uploading mark | _(uploading: Game Name…)_, Add Game, Manage Games, Open Dashboard, Sign Out, Quit |
| Error | Error mark | _(error message)_, Retry, Add Game, Manage Games, Open Dashboard, Sign Out, Quit |
| Limit reached | Error/warn mark | "Free upload limit reached", Add Game, Manage Games, Open Dashboard, Sign Out, Quit |
| Not authenticated | Dim mark | Sign In, Quit |
| No games configured | Normal mark | Add Game, Open Dashboard, Sign Out, Quit |

**Add Game** — native dialog to pick a watch path, enter a `game_id`, and enter the events filename (e.g. `lt_fo4_events.jsonl`). Appends to `config.toml` and registers the events file with the live watcher immediately.  
**Manage Games** — opens `config.toml` in the OS default text editor (changes take effect on restart for MVP).

---

## Game Identity

Game ID comes from config — the `game_id` in the `[[games]]` block whose `events_file` matches the file that triggered the WRITE event. The Client never infers `game_id` from filename or JSONL content.

Each game maps to exactly one events file:

```
lt_fo4_events.jsonl     → game_id: fallout4   (from config)
lt_stardew_events.jsonl → game_id: stardew    (from config)
```

Matching is by exact base filename against `events_file` in config — no glob, no pattern.

---

## Environment Variables

```env
# Override config for development
LUDOTRACE_CORE_URL=http://localhost:8080
LUDOTRACE_LOG_LEVEL=debug
```

---

## Build Targets

```makefile
build-mac:
    GOOS=darwin GOARCH=amd64 go build -o dist/ludotrace-mac ./cmd/ludotrace
    GOOS=darwin GOARCH=arm64 go build -o dist/ludotrace-mac-arm ./cmd/ludotrace

build-windows:
    GOOS=windows GOARCH=amd64 go build -o dist/ludotrace.exe ./cmd/ludotrace

build-linux:
    CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o dist/ludotrace-linux ./cmd/ludotrace
    # requires: libgtk-3-dev libappindicator3-dev (for systray) + gnome-keyring (for go-keyring)

build-all: build-mac build-windows build-linux
```

---

## Key Principles

- **Dumb by design** — no processing, no LLM calls, no business logic
- **Trust Core** — if Core says 202, delete the file. Don't second-guess.
- **Never lose a session** — queue locally if offline, retry until success
- **Token security** — never store token in config file or plaintext. OS keychain only.
- **Low resource footprint** — this runs in the background always. Minimize CPU and memory.
- **Cross-platform first** — test on macOS and Windows. Linux is a bonus.

---

## MVP Scope

Out of scope: installer / setup wizard, multiple accounts, manual upload UI, session history in tray. (Auto-update shipped `v0.1.0` — see `STATUS.md`, no longer out of scope.)

Ship the watcher. Ship the upload. Ship the tray icon. Validate the end-to-end flow works for one user (you) before adding anything else.

## Issues & PRs

GitHub, single remote (`github.com/ludotrace/client`). Issues and PRs both via `gh` (`gh issue create/list`, `gh pr create`) — pass `--repo ludotrace/client` if running outside a clone.
