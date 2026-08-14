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
- Retire a session — delete the temp file, advance the offset, drop the queue entry — on a
  202 from Core, or on a rejection Core will never accept (400, too large). Never on a
  transient failure
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
- **Inactivity flush** closes the open session when the events file has gone quiet for
  **12 minutes**. This is the generic "the player stopped" signal, and it is what closes the
  final session of a play period — including one that ended with no clean `session_end` at
  all (crash, or quit with no final save). For Fallout 4 it closes *every* session.

  `orphanThreshold` in `internal/session/session.go` is the single definition of that value —
  the PRD, the architecture spine's AD-2, and the mod scaffolding guide all quote it. Change
  it there and update those together; never add a second place that states the number.
- **Client shutdown** is the second closing boundary: an open session is extracted and
  enqueued as the Client exits, rather than left for an indefinite next launch. Extraction
  only — the upload worker is already stopping, so it goes out on the next run.

  These two are the *only* boundary signals, and deliberately so. The sharper alternative is
  observing the game process, which means enumerating the process table from a background
  tray app — outside the configured watch path, hard to state honestly in a privacy policy,
  and awkward for antivirus heuristics. Carry-forward makes the extra precision unnecessary.
- **Carry-forward.** Bytes that arrive with no session open — the region between the offset
  and the first `session_start` — **are not a session and never become one on their own.**
  A flush leaves them behind whenever the player resumes after it fired: their opener is
  already uploaded, so the region has none.

  They are **held**, with the offset unmoved, and prepended to the next session as its
  lead-in. Holding is close to lossless: the bytes stay on disk and nothing depends on the
  Client surviving. Never upload a held fragment on its own to "not lose it" — it is not a
  candidate for its own insight, so it would spend an inference run on something the model
  cannot place.

  A hold is released only when it **gains an opener** (a `session_start` arrives — upload it
  with the held bytes attached, in place, as one contiguous region) or when it **exceeds
  `holdCap`** (4 MiB raw), so it cannot grow without bound. No minimum-event floor and no
  age-based expiry: a flat floor cannot work, because the same two minutes may be dense or
  empty depending on the game and the Client cannot tell.
- **Sidecar offset** at `<config_dir>/offsets/<game_id>.offset`, advanced when a session is
  retired — on a 202, or on a permanent rejection whose bytes Core will never accept. It only
  ever moves forward: uploads are served newest-first, so end offsets come back out of order
  and a rewind would re-extract sessions the pipeline has already finished with. A missing
  offset restarts from byte 0.

  **The extractor is the one thing that may move the mark backward**, and only when the
  events file is shorter than the stored offset — the file was replaced, not appended to. It
  writes the reset straight to the sidecar. Nothing else can: every end offset the shorter
  file produces is below the old mark, so an unreset mark would swallow them all and each
  pass would re-upload the same sessions.
- **Missing events file** — watch the parent directory, promote to a file watch on CREATE.
- **Durable queue** — extraction enqueues; a separate worker uploads. The watcher must never
  call upload directly.

## Capture context

Every upload carries a `capture_context` form field describing *how* the region was captured,
so the model can say "the opening of this run is missing" instead of hedging (core#116).
`internal/capture` holds the shape; `internal/session` fills it in.

| Field | Meaning |
|---|---|
| `opener` | `present` / `absent` — is the `session_start` that began this run inside the upload? |
| `closed_by` | `superseded`, `idle_timeout`, `client_shutdown`, `size_cap` |
| `gap_before` | Seconds of idle between a carried-forward lead-in and the session it leads into. Omitted when there is no lead-in, **and when the gap cannot be measured** |
| `event_count`, `span_s` | What the Client counted over the region it uploaded |

**The enums are a contract with `core/internal/capture`.** Core validates every value and
rejects the whole upload on anything off-enum, non-integer, partial, or unknown — so a value
added here without the matching Core change fails every upload that sends it.

Sending no capture context at all is always valid: Core leaves the block out entirely. That
is what a queue item enqueued before the field existed does.

### Reading `wall_time`

**`wall_time` is whatever the mod chose to put there.** The capture layer is open, so the
field arrives in whatever shape a given author could reach from their modding API. The spec
asks for an ISO 8601 UTC instant, but that is guidance to mod authors, **not a guarantee to
this code** — treat any strict reading of it as a bug waiting to happen.

`internal/session/wallclock.go` is the single place that interprets it, on one rule:
**parse generously, interpret narrowly.**

- Accept as many shapes as can be recognized without guessing — separator, precision, and
  timezone all vary, and none of that changes what a difference means. Add layouts freely.
- Use only *differences* between two readings, never an absolute value, so an unknown origin
  or unstated timezone costs nothing.
- Never subtract readings that are not evidently the same kind. A calendar instant and a bare
  counter differ by ~9 orders of magnitude; subtracting them yields a confident, meaningless
  number.
- When anything is unclear, **report no measurement.** `gap_before` is omitted rather than
  sent as `0` — zero is a reported gap the model may read as a real boundary, while absent
  says only that nothing was measured. A negative delta means a counter reset, not time
  running backwards, so it is declined rather than clamped.

**Do not add heuristics that infer meaning from magnitude** — guessing epoch-vs-counter or
seconds-vs-milliseconds from how big a number looks. That trades a visible "unknown" for an
invisible wrong answer, and an unknown is the one this field can afford.

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
