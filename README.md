# LudoTrace Client

Background daemon that watches a game's event log, extracts sessions, and uploads them to [LudoTrace Core](https://github.com/ludotrace/core) for LLM processing.

Part of the [LudoTrace](https://ludotrace.github.io) project.

---

## How it works

```
Game mod appends events to lt_<game>_events.jsonl
        ↓
Client detects writes (fsnotify, 2s debounce)
        ↓
Extracts session_start → session_end pairs
        ↓
Authenticates with Core, uploads each session as gzip JSONL
        ↓
Advances sidecar offset on 202 — never reprocesses uploaded sessions
```

Sessions that end abruptly (crash, force-quit) are uploaded after 12 minutes of inactivity.

---

## Install

Download `ludotrace.exe` from the [latest CI build](https://github.com/ludotrace/client/actions) under **Artifacts**.

Create `%APPDATA%\ludotrace\config.toml`:

```toml
core_url = "https://core.ludotrace.com"

[[games]]
game_id     = "fallout4"
watch_path  = "C:\\Users\\You\\Documents\\My Games\\Fallout4"
events_file = "lt_fo4_events.jsonl"
```

Run `ludotrace.exe` — it appears in the system tray. Click **Sign In** to authenticate.

### Unsigned binary warnings

Release binaries are **not code-signed**. Your OS will warn you before running them — this is expected, not a sign of malware. Code signing is on the roadmap (see [#18](https://github.com/ludotrace/client/issues/18)).

**Windows (SmartScreen):**

1. You'll see "Windows protected your PC".
2. Click **More info**.
3. Click **Run anyway**.

**macOS (Gatekeeper):**

Either:

- Right-click (or Control-click) the binary → **Open** → confirm **Open** in the dialog, or
- Run in Terminal: `xattr -d com.apple.quarantine ./ludotrace-mac-*`

---

## Build

Requires Go 1.25+.

```bash
# Windows (from any platform)
make build-windows       # → dist/ludotrace.exe

# macOS
make build-mac           # → dist/ludotrace-mac-x64
make build-mac-arm       # → dist/ludotrace-mac-arm64

# Linux (requires libgtk-3-dev libappindicator3-dev)
make build-linux         # → dist/ludotrace-linux

# Tests
make test
```

---

## Configuration

| Key | Default | Description |
|-----|---------|-------------|
| `core_url` | `https://core.ludotrace.com` | Core API base URL |
| `games[].game_id` | — | Identifier used by Core to select the prompt template |
| `games[].watch_path` | — | Directory containing the events file |
| `games[].events_file` | — | Filename of the append-only events log |

**Environment overrides:**

```env
LUDOTRACE_CORE_URL=http://localhost:8080   # useful for local Core dev
LUDOTRACE_LOG_LEVEL=debug                  # JSON logs to stderr
```

---

## Package layout

```
cmd/ludotrace/   — main entrypoint
internal/
  auth/          — sign-in flow, JWT lifecycle (Core-proxied Clerk)
  keychain/      — opaque token storage (OS keychain)
  config/        — TOML load/save, game config, path helpers
  watcher/       — fsnotify wrapper, debounce, startup scan
  session/       — session extraction, orphan detection, sidecar offset
  queue/         — durable upload queue (queue.json), atomic flush
  uploader/      — gzip multipart POST, retry, error classification
  tray/          — system tray icon and menu (Windows/macOS/Linux)
```

---

## State files

All state lives in the OS config directory (`%APPDATA%\ludotrace` on Windows, `~/.config/ludotrace` on macOS/Linux):

| File | Purpose |
|------|---------|
| `config.toml` | Game paths and Core URL |
| `queue.json` | Durable upload queue — survives restarts |
| `offsets/<game_id>.offset` | Byte position after last uploaded session_end |
| `ludotrace.lock` | Singleton enforcement |
