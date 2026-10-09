# Threat model

## What this project does and where untrusted input enters

LudoTrace Client is a background desktop daemon (macOS, Windows, Linux). It watches a game's
append-only JSONL events file, splits it into play sessions, and uploads each session to the
LudoTrace server ("Core") with the user's credentials. Entry points, most exposed first:

- **The events file** (`internal/session`, `internal/watcher`). Third-party game mods write it, so
  treat its contents as untrusted: arbitrary bytes, partial lines, huge lines, malformed JSON,
  hostile `wall_time` values, and a file that is truncated or replaced between reads.
- **The sign-in loopback listener** (`internal/auth`). During sign-in it listens on
  `127.0.0.1:<random port>` and accepts a redirect carrying `token` and `state`. Any local
  process or any web page in the user's browser can reach it.
- **The self-updater** (`internal/updater`, `cmd/ludotrace/main.go`). It fetches a JSON manifest
  over HTTPS, downloads the binary it names, checks the manifest's SHA-256, stages it in the
  config directory, and executes it on the next start.
- **HTTP responses from Core** (`internal/auth`, `internal/uploader`, `internal/updater`): token
  exchange, upload status codes and bodies, and the update manifest.
- **Local state files** in the user's config directory: `config.toml`, `queue.json`, and
  `offsets/<game_id>.offset`. Another process running as the same user could tamper with them,
  which is lower priority (see below).
- **Steam library discovery** (`internal/steam`, Windows only) reads Steam's install location
  from the registry and parses its library files from disk.

## Components that matter most / least

- Most: token handling (keychain storage, the opaque token → short-lived JWT exchange, anything
  that could leak a token to a log, a file, or another origin), the loopback callback (state
  verification, CSRF, token injection), and the updater's path from download to execution.
- High: session extraction and offset bookkeeping. A crafted events file must never crash the
  daemon, make it read outside the configured file, grow memory without bound, or upload data
  from outside the configured events file.
- Lower: the tray UI, the Add Game dialog, autostart registration, and `internal/tracing`.
- Out of scope: third-party modules under the Go module cache.

## How to exercise it

- `go test -race ./...` from `/src`. Package tests use temporary files and `httptest` servers and
  do not need a running Core.
- `dist/ludotrace-linux-headless` runs without a display (`dist/ludotrace-linux` needs one, or
  `--headless`). `LUDOTRACE_CORE_URL` and `LUDOTRACE_APP_URL` (`internal/config`) point it at a
  local test server.
- The session extractor in `internal/session` is the easiest target for fuzzing with crafted
  JSONL.

## How you rate severity

- Critical: remote code execution, including making the updater run a binary that Core did not
  publish.
- High: token or JWT disclosure, a token accepted from the wrong party during sign-in, or data
  outside the configured events file read or uploaded.
- Medium: a crash or unbounded resource use triggered by events file contents, and session data
  silently lost or duplicated by crafted input.
- Low: anything that needs an attacker already running as the same OS user, and DoS that needs
  unrealistic resources.

## Anything to leave alone

- No Cloudflare Access or similar network gate sits in front of Core. That is deliberate (a
  service token inside a distributed binary is not a secret), so please do not report it.
- This image builds and tests the Linux binaries only. Windows-only files (`*_windows.go`) are
  in scope as source but are not compiled here.
