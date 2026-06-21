# LudoTrace Client — Development Spec

Single source of truth for implementing the Client. Read alongside `CLAUDE.md` for stack
decisions and `internal/ARCHITECTURE.md` for the full system picture. Upload contract is
defined by `core/SPEC.md` — when the two disagree, Core's upload spec wins.

The Client is intentionally dumb: **watch events file → extract sessions → authenticate → compress → upload → advance offset.**
No processing, no LLM calls, no business logic. Session boundary detection is the only logic it owns.

---

## Entry Point

One binary:

```
cmd/ludotrace/main.go   — starts config load, auth, watcher, and tray event loop
```

Runs as a long-lived background process with a system tray icon. No HTTP server except the
transient localhost callback during the OAuth flow.

---

## Config

TOML file in the OS config directory:
- Linux/macOS: `~/.config/ludotrace/config.toml`
- Windows: `%APPDATA%\ludotrace\config.toml`

```toml
core_url = "https://core.ludotrace.com"

[[games]]
game_id     = "fallout4"
watch_path  = "C:/Users/Username/Documents/My Games/Fallout4"
events_file = "lt_fo4_events.jsonl"

[[games]]
game_id     = "stardew"
watch_path  = "C:/Users/Username/AppData/Roaming/StardewValley/Saves"
events_file = "lt_stardew_events.jsonl"
```

**Validation on startup**
- `core_url` must be a valid absolute URL → fatal if missing/invalid
- Each `game_id` should match a game Core supports; an unknown `game_id` is accepted locally
  (Core rejects it at upload with `400 unknown_game`, surfaced as an error state)
- `watch_path` must exist and be a directory → log a warning and skip that game if not
- `events_file` is the base filename of the live event stream; full path is `watch_path/events_file`

`LUDOTRACE_CORE_URL` env var overrides `core_url` for development.

---

## Filename & Game Identity Convention

Per `internal/ARCHITECTURE.md`, mods write a single append-only live event stream:

```
lt_<game>_events.jsonl   — append-only event stream; never deleted by the mod
```

- The Client watches this file for modifications and extracts sessions from it — see Read Position Tracking below.
- `game_id` comes from the matched `[[games]]` block in config — **never** parsed from filename or JSONL content.

---

## Auth Flow

The Client authenticates with Clerk via browser-based OAuth and stores the result in the OS
keychain. Core only ever sees the resulting bearer token and verifies it; see
`core/SPEC.md` → Auth Middleware.

```
1. On startup, read token material from OS keychain.
2. If a usable access token exists and is unexpired → use it.
3. If it is expired but a refresh token exists → refresh silently (no browser).
4. If there is no token, or refresh fails → enter "Not authenticated":
   a. Tray shows "Sign In".
   b. On user action, open the system browser to the Clerk OAuth URL.
   c. Start a localhost HTTP server on a random port for the redirect callback.
   d. Receive the auth code, exchange it for access + refresh tokens.
   e. Persist tokens in the OS keychain; tear down the localhost server.
5. Use the access token for all Core API requests (Authorization: Bearer <token>).
6. Refresh proactively shortly before access-token expiry.
```

```go
type AuthClient interface {
    // GetToken returns a currently-valid access token, refreshing if needed.
    // Returns ErrNotAuthenticated if interactive sign-in is required.
    GetToken(ctx context.Context) (string, error)
    Refresh(ctx context.Context) error
    Logout() error
}
```

**Token storage:** OS keychain via `zalando/go-keyring`. Never written to the config file or
any plaintext on disk. Store access token, refresh token, and expiry under namespaced keys.

**On `401` from Core:** treat the access token as stale — attempt one `Refresh()`, retry the
request once. If refresh fails, transition to "Not authenticated", keep the session file
queued, and surface the tray "Sign In" state. Never drop a session because auth lapsed.

> **The hard part — headless refresh.** Clerk's short-lived session JWTs are normally
> refreshed by its frontend SDK, which the Client does not run. The Client must hold an
> OAuth refresh token (or equivalent long-lived credential) and exchange it itself. The
> exact Clerk mechanism is unresolved — see Decisions Not Yet Made.

---

## Watcher

`fsnotify` watches the live events file for each configured game.

```go
for _, game := range cfg.Games {
    eventsPath := filepath.Join(game.WatchPath, game.EventsFile)
    watcher.Add(eventsPath)
}

on WRITE(path):
    game := matchGame(path)          // base filename matches game.EventsFile
    if game == nil { continue }
    debounce(game, 2s, func() {
        session.ExtractAndEnqueue(game) // see Read Position Tracking
    })
```

**Key behaviours**
- **WRITE events, not CREATE.** The events file is long-lived and append-only; the Client
  watches for modifications to it, not creation of new files.
- **Debounce.** A burst of game events fires many WRITE notifications in quick succession.
  Wait for 2 seconds of inactivity before triggering session extraction, to avoid processing
  partial lines mid-write.
- **Startup read.** On startup, trigger a session extraction pass for each configured game
  immediately — the events file may contain unprocessed sessions from while the Client was
  down. Extraction reads from the persisted sidecar offset (0 if missing), so already-
  uploaded sessions are never re-uploaded.
- **Missing file.** If the events file does not exist yet (game never played), register the
  watch on the parent directory instead and promote to a file watch on CREATE of the events
  file.

---

## Read Position Tracking

lt-client tracks its read position per game using a sidecar offset file stored in the config directory:

```
<config_dir>/ludotrace/offsets/<game_id>.offset
```

The file contains a single integer: the byte offset after the last `session_end` line successfully uploaded to Core. On `202`, the offset advances and the sidecar is written atomically (write to `.tmp`, rename).

**Session extraction** (run after each debounced WRITE and on startup):

```go
offset := loadOffset(game.GameID)   // 0 if sidecar missing — full reprocess
lines, newOffset := readLinesFrom(eventsPath, offset)

// Scan for complete session_start → session_end pairs
sessions := extractSessions(lines, game.GameID)

for _, s := range sessions {
    tmpPath := writeTempSession(s.Lines)
    queue.Enqueue(QueueItem{
        GameID:    game.GameID,
        TempPath:  tmpPath,
        EndOffset: s.EndOffset,   // offset to write to sidecar on 202
    })
}
```

**Orphan sessions.** A `session_start` with no following `session_end` and whose last event timestamp is >30 minutes old is treated as complete and uploaded as-is. A session still within the 30-minute window is left for the next read cycle.

**Offset advancement.** The uploader writes the sidecar offset after receiving `202` — not before, not on enqueue. If the Client crashes between `202` and the sidecar write, the session is reprocessed on next startup (Core receives a duplicate; acceptable at MVP — deduplicate by `session_start` timestamp in a future iteration).

**Alternative deferred:** truncation-after-upload eliminates the sidecar entirely but is destructive for standalone users. May be offered as `truncate_after_upload = true` in config in future.

---

## Upload Queue

A durable, in-process queue decouples detection from upload so nothing is lost on crash,
restart, or while Core is unreachable.

- **Durability.** Persist the queue to `<config_dir>/queue.json` as a JSON array of
  `{"game_id": "...", "tmp_path": "...", "end_offset": 0, "attempts": 0}` objects. Load it
  on startup before the watcher or startup extraction runs. Flush to disk after every enqueue,
  dequeue, and attempt-count update. `tmp_path` is the extracted session temp file (written
  by the session extractor); `end_offset` is the byte position to write to the sidecar on
  `202`. The temp file stays on disk until upload confirms deletion.
- **Offline handling.** If Core is unreachable, keep the item queued and retry with backoff
  when connectivity returns. Never delete an un-uploaded file.
- **Ordering.** FIFO; one in-flight upload at a time keeps resource use low and makes the
  tray state unambiguous.
- **Dedup.** The primary dedup guard is the sidecar offset — the session extractor only
  reads from the last committed offset, so already-uploaded sessions are never re-extracted.
  A `Contains(tmpPath string) bool` method on the queue provides a secondary guard: the
  extractor checks it before enqueuing so a temp file already in the queue is not added
  twice (relevant if extraction runs again before the upload worker drains the queue).

---

## Upload

```go
// POST {core_url}/v1/upload   (contract owned by core/SPEC.md)
func Upload(ctx context.Context, gameID, filePath, token string) (jobID string, err error)
```

**Request construction**
- `multipart/form-data` with two parts:
  - `game_id`      — the matched game_id
  - `session_file` — the session JSONL, **gzip-compressed** (Core requires gzip and
    decompresses server-side; see `core/SPEC.md` → POST /v1/upload)
- Header: `Authorization: Bearer <token>`
- Compress by streaming the file through `gzip.Writer` into the multipart part — do not
  buffer the whole compressed payload in memory.

**Response handling**

| Status | Meaning | Action |
|--------|---------|--------|
| `202` | accepted, body `{ "job_id": "..." }` | delete temp session file; advance sidecar offset; mark item done |
| `401` | token stale/invalid | refresh + retry once; else re-auth, keep queued |
| `413` | file too large (compressed or decompressed cap) | do **not** retry; log + tray error; keep file (don't silently lose it) |
| `429` | `upload_limit_reached` (free tier) | do not retry immediately; surface "limit reached" in tray; keep queued for later |
| `400` | `unknown_game` / `invalid_gzip` / etc. | do not retry; log the specific error; keep file; tray error |
| `5xx` / network | transient | retry with backoff, then leave queued (offline path) |

**Delete on confirm only.** Trust Core: if and only if Core returns `202`, delete the local
session file. Never second-guess a `202`; never delete on any other outcome.

**Retry/backoff.** Up to 3 immediate attempts with exponential backoff on transient
failures; after that the item remains in the durable queue for the next connectivity/poll
cycle rather than being dropped.

---

## System Tray

The daemon is game-agnostic and runs as a singleton. The tray is the primary management
surface — users add and manage game watch paths here rather than hand-editing config.

**States**

| State | Icon | Menu |
|-------|------|------|
| Idle, authenticated | Normal mark | Add Game, Manage Games, Open Dashboard, Sign Out, Quit |
| Uploading | Uploading mark | _(uploading: Game Name…)_, Add Game, Manage Games, Open Dashboard, Sign Out, Quit |
| Error | Error mark | _(error message)_, Retry, Add Game, Manage Games, Open Dashboard, Sign Out, Quit |
| Limit reached | Error/warn mark | "Free upload limit reached", Add Game, Manage Games, Open Dashboard, Sign Out, Quit |
| Not authenticated | Dim mark | Sign In, Quit |
| No games configured | Normal mark | Add Game, Open Dashboard, Sign Out, Quit |

**Menu actions**

- **Add Game** — opens a small native dialog to pick a watch path, enter a `game_id`, and
  enter the events filename (e.g. `lt_fo4_events.jsonl`). Appends a new `[[games]]` block to
  `config.toml` and registers the path with the running watcher immediately (no restart
  required).
- **Manage Games** — opens `config.toml` in the OS default text editor. Changes take
  effect on next startup (MVP); live reload is out of scope.
- **Open Dashboard** — opens `{core_url}/dashboard` in the system browser.
- **Sign Out** — calls `AuthClient.Logout()`, clears keychain tokens, transitions to "Not
  authenticated".
- **Retry** — re-attempts the head of the upload queue immediately.

**State derivation.** Derived from queue + auth state: any in-flight upload → Uploading;
last attempt failed → Error; `ErrNotAuthenticated` → Not authenticated; queue empty and
no games configured → No games configured; otherwise Idle.

**Singleton enforcement.** Only one instance may run at a time. On startup, acquire an
exclusive lock on `<config_dir>/ludotrace.lock` using `os.OpenFile` with `O_EXCL`. If the
lock is already held, log the conflict and exit immediately with a clear message. Release
the lock on process exit.

> **MVP note.** The "Add Game" dialog is the minimum viable alternative to hand-editing
> config. It can be as simple as a sequence of `dialog.AskString` prompts from a small
> cross-platform dialog library (e.g. `sqweek/dialog`). Full "Manage Games" UI is out of
> scope for MVP — opening the config file in a text editor is sufficient.

---

## Logging

Use Go's standard `log/slog` in structured JSON mode for all log output (file + line
implied by `slog`'s default source attribution). Default level: `INFO`.

| Level | Examples |
|-------|---------|
| `DEBUG` | file event received, queue flush, token refresh attempt |
| `INFO` | file enqueued, upload succeeded, auth state change, startup/shutdown |
| `WARN` | watch path not found at startup (skipped), unknown game_id accepted locally |
| `ERROR` | upload failed (with status + body snippet), queue write failure, lock conflict |

`LUDOTRACE_LOG_LEVEL=debug` enables `DEBUG` output. Log to stderr; the OS process
supervisor (launchd, Windows Task Scheduler, etc.) handles redirection.

Never log token values, file contents, or PII.

---

## Graceful Shutdown

On `SIGTERM` or `SIGINT`:

1. Stop accepting new watcher events.
2. If an upload is in-flight, let it complete (it has already started — aborting mid-POST
   risks a silent Core failure with no `202` confirmation).
3. Flush the queue to disk.
4. Release the singleton lock file.
5. Exit.

Do not drain the entire queue on shutdown — remaining items are durable on disk and will
resume on next startup. Only the single in-flight upload is waited on.

```go
// Wire in main.go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
```

---

## Environment Variables

```env
# Development overrides only — production config lives in config.toml + keychain
LUDOTRACE_CORE_URL=http://localhost:8080
LUDOTRACE_LOG_LEVEL=debug
```

---

## Package Layout

```
client/
├── cmd/
│   └── ludotrace/main.go    — wire config, auth, watcher, queue, tray; run event loop
├── internal/
│   ├── config/              — TOML load + validation, env overrides, Add Game writer
│   ├── auth/                — Clerk OAuth flow, token refresh, ErrNotAuthenticated
│   ├── keychain/            — token storage (zalando/go-keyring)
│   ├── watcher/             — fsnotify wrapper, WRITE-event watch, debounce, startup trigger
│   ├── session/             — session extraction (session_start→session_end), orphan detection, sidecar offset read/write, temp file writer
│   ├── queue/               — durable upload queue (queue.json), Contains(), enqueue, dequeue
│   ├── uploader/            — gzip + multipart POST to Core, response handling, backoff, offset advancement on 202
│   └── tray/                — systray icon + menu, state derivation, Add Game dialog
├── assets/                  — icon.png, icon_error.png, icon_uploading.png
├── Makefile                 — cross-platform build targets (mac/win/linux)
├── CLAUDE.md
└── SPEC.md
```

**Config dir layout at runtime**

```
<config_dir>/ludotrace/
├── config.toml          — game watch paths and core_url
├── queue.json           — durable upload queue
├── ludotrace.lock       — singleton lock (O_EXCL; released on exit)
└── offsets/
    ├── fallout4.offset  — byte offset after last uploaded session_end
    └── stardew.offset
```

**Build notes**

- `getlantern/systray` on Linux requires CGo and the `libgtk-3-dev` and
  `libappindicator3-dev` system packages. The Linux build target must enable CGo
  (`CGO_ENABLED=1`). macOS and Windows do not require additional system libraries.
- `zalando/go-keyring` on Linux uses the system Secret Service (D-Bus); install
  `gnome-keyring` or `keepassxc` (with Secret Service enabled) for dev/test.

---

## Key Principles

- **Dumb by design** — no processing, no LLM calls, no business logic.
- **Trust Core** — if Core says `202`, delete the file. Don't second-guess.
- **Never lose a session** — durable queue; retry until success; keep files on every
  non-`202` outcome.
- **Token security** — OS keychain only, never plaintext.
- **Low footprint** — runs always in the background; one upload in flight; minimal CPU/mem.
- **Cross-platform first** — test macOS and Windows; Linux is a bonus.

---

## MVP Scope (out)

Auto-update, installer/setup wizard, multiple accounts, manual upload UI, session history in
tray. Ship the watcher, the upload, and the tray icon; validate end-to-end for one user
first.

---

## Decisions Not Yet Made

- **Client token acquisition & headless refresh (blocker).** Clerk session JWTs are
  short-lived (~60s) and refreshed in normal apps by Clerk's frontend SDK, which the
  headless Client does not run. The Client must acquire a long-lived credential at sign-in
  and refresh access tokens itself. Open: which Clerk mechanism — OAuth authorization-code
  flow with a refresh token, a Clerk "long-lived" session, or a machine/API token — and the
  exact token endpoints, scopes, and refresh-token rotation handling. This gates the `auth`
  package. Core is unaffected (it verifies whatever valid JWT it receives); cross-referenced
  in `core/SPEC.md` → Decisions Not Yet Made.
- **Refresh-token expiry / re-auth UX.** When the long-lived credential itself expires or is
  revoked, the background daemon must prompt re-auth without a foreground window. MVP plan:
  drop to "Not authenticated" tray state and require the user to click Sign In. Confirm this
  is acceptable vs. a notification.
- **Debounce tuning.** The 2-second quiet-period debounce before reading new lines is an
  initial estimate. Revisit based on observed write cadence in practice.
- **Auth callback port/redirect registration.** The localhost callback uses a random port;
  confirm Clerk allows a wildcard/loopback redirect URI for installed-app OAuth.
- **"Add Game" dialog library.** `sqweek/dialog` is suggested for MVP but has limited
  cross-platform polish. Revisit if a richer multi-field form is needed.
