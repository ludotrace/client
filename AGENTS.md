# AGENTS.md — LudoTrace Client

Canonical instructions for this repo. Root `../AGENTS.md` covers the cross-repo workflow
(git, worktrees, issues/PRs, doc discipline).

## What this is

A lightweight background daemon that watches a game's append-only live events file, extracts
complete sessions, and uploads them to Core.

**It is intentionally dumb.** No UI beyond a tray icon, no LLM calls, no business logic:
watch → extract → authenticate → compress → upload → advance offset. Session boundary
detection is the only logic it owns.

`STATUS.md` records what is actually built. Read it before starting work, update it after.

## Responsibilities

- Run as a singleton background daemon, one instance per OS session
- Watch each configured game's append-only events file for WRITE events — game-agnostic
- Extract play sessions and track read position per game via a sidecar offset file
- Authenticate with Core via browser-based sign-in; store the token in the OS keychain
- Upload sessions through a durable queue, with the correct `game_id`
- Delete the temp file and advance the offset **only** on a 202 from Core
- Surface state through a tray icon, with Add Game as the primary setup action
- Run on macOS, Windows, and Linux

## Tech stack

| | |
|---|---|
| Language | Go |
| File watching | `fsnotify` |
| HTTP | stdlib `net/http` |
| Auth | Browser sign-in via Core, loopback callback |
| Token storage | OS keychain (`zalando/go-keyring`) |
| System tray | `getlantern/systray` |
| Config | TOML in the OS config directory |

## Project structure

```
client/
├── cmd/ludotrace/    — entrypoint; wires config, auth, watcher, queue, tray
├── internal/
│   ├── config/       — TOML load + validation, env overrides, Add Game writer
│   ├── auth/         — sign-in flow, token exchange, ErrNotAuthenticated
│   ├── keychain/     — token storage
│   ├── watcher/      — fsnotify wrapper, debounce, startup trigger
│   ├── session/      — session extraction, orphan detection, sidecar offset
│   ├── queue/        — durable upload queue (queue.json)
│   ├── uploader/     — gzip + multipart POST, backoff, offset advancement
│   └── tray/         — icon, menu, state derivation, Add Game dialog
├── assets/           — tray icons
└── Makefile
```

## Auth flow

**The Client never calls Clerk directly** — Core proxies the entire flow. This is a
deliberate boundary (architecture spine AD-3): exactly two credential paths exist
system-wide, and this is the machine/headless one.

1. Check the OS keychain for a valid opaque token.
2. If absent or expired: open the system browser to Core's sign-in, start a loopback listener
   on a random port, and receive a Core-issued opaque token on the redirect. Store it in the
   keychain.
3. On each upload, exchange the opaque token for a short-lived Core-signed JWT.
4. Use that JWT as the bearer for that request only — **never cache it past its lifetime.**

## Watcher logic

The mod writes one append-only events file per game. Watch it for WRITE events and extract
complete sessions.

- **WRITE, not CREATE** — the file is long-lived and append-only.
- **Debounce 2s** — coalesce burst writes so a partial line is never read.
- **`session_start` is the only structural boundary.** A new one closes the previous session
  and flushes it.
- **`session_end` is NOT a boundary** — it is opaque payload buffered into the current
  session. Games emit it on wildly different cadences (Fallout 4 writes one per *save*, so
  one play session contains many), so treating it as a terminator would split or drop data
  based on a game-specific quirk. Keep this game-agnostic.
- **Inactivity flush** closes the open session when the events file has gone quiet. This is
  the generic "the player stopped" signal, and it is what closes the final session of a play
  period — including one that ended with no clean `session_end` at all (crash, or quit with
  no final save). For Fallout 4 it closes *every* session.

  ⚠️ **The threshold is currently wrong and the fix is launch-blocking** — the code uses 10
  minutes, the spec and PRD say 30. See **#69** before changing anything in this area.
- **Sidecar offset** at `<config_dir>/offsets/<game_id>.offset`, advanced only on 202. A
  missing offset restarts from byte 0.
- **Missing events file** — watch the parent directory, promote to a file watch on CREATE.
- **Durable queue** — extraction enqueues; a separate worker uploads. The watcher must never
  call upload directly.

## Game identity

`game_id` comes from config — the `[[games]]` block whose `events_file` matches the file that
triggered the WRITE event, by exact base filename. No glob, no pattern.

**Never infer `game_id` from a filename or from JSONL content.**

## Key principles

- **Dumb by design** — no processing, no LLM calls, no business logic.
- **Trust Core.** If Core says 202, delete the file. Don't second-guess it.
- **Never lose a session.** Queue locally when offline and retry.
- **Token security** — OS keychain only. Never the config file, never plaintext.
- **Low resource footprint** — this runs in the background permanently.
- **Never add a network-layer gate** (Cloudflare Access or similar) in front of Core. A
  service token shipped inside a distributed desktop binary is not a secret, and a
  hostname-wide gate breaks sign-in and every upload.
- **Cross-platform first** — test on macOS and Windows; Linux is a bonus.

## Config file

Lives in the OS config directory (`~/.config/ludotrace/config.toml`,
`%APPDATA%\ludotrace\config.toml` on Windows):

```toml
core_url = "https://core.ludotrace.com"

[[games]]
game_id     = "fallout4"
watch_path  = "C:/Users/Username/Documents/My Games/Fallout4"
events_file = "lt_fo4_events.jsonl"
```

**Never edit a real user config, queue, or offset file to test something** — it corrupts the
very state you're trying to observe. See root `AGENTS.md` § Debugging discipline.

## Environment variables

```env
LUDOTRACE_CORE_URL=http://localhost:8080   # dev override
LUDOTRACE_LOG_LEVEL=debug
QUEUE_MAX_AGE_DAYS=30   # max age before a queued session is dropped, oldest first;
                        # bounds queue growth for a chronically over-quota player
```

## Build

```bash
make build-all     # mac (amd64 + arm64), windows, linux
```

The Linux build needs `libgtk-3-dev`, `libappindicator3-dev` (systray) and `gnome-keyring`.

The Windows build is `-H=windowsgui` and has no console, so it emits nothing to a terminal.
**When something doesn't work there, restoring a signal is the first task** — a fix without
one is a guess.

## Issues & PRs

GitHub, single remote (`github.com/ludotrace/client`). Issues and PRs both via `gh` — pass
`--repo ludotrace/client` when running outside a clone.

The `pre-open-source` label marks issues that must be resolved before this repo is flipped
public: `gh issue list --label pre-open-source`.
