# Client STATUS

What is actually built, validated, or just designed. Not a spec or wish list.
Cross-referenced against code in this repo and `internal/features.md`.

---

## Verified end-to-end

Confirmed working via manual end-to-end test or observed in production.

- **Singleton lock** — `cmd/ludotrace/main.go:54`: `O_CREATE|O_EXCL` on `ludotrace.lock`; exits cleanly if lock is held.
- **Config load + validation** — `internal/config/config.go:Load()`: TOML parse, URL validation, `filterGames` skips missing watch paths with WARN, `LUDOTRACE_CORE_URL` / `LUDOTRACE_APP_URL` env overrides applied.
- **File watcher (fsnotify, 2 s debounce)** — `internal/watcher/watcher.go`: watches WRITE+CREATE events; if events file absent at startup, watches parent directory and promotes to file watch on CREATE (`watcher.go:52-65`). Debounce timer reset on burst (`watcher.go:149-150`).
- **Startup extraction** — `internal/watcher/watcher.go:Start()`: immediately triggers `cb(gameID)` for each game whose events file exists before entering the event loop (`watcher.go:74-78`).
- **Session extraction + sidecar offset** — `internal/session/session.go:Extract()`: reads from persisted byte offset, splits on `session_start` only (session_end is opaque payload), uses custom `scanRawLines` to avoid 1-byte drift on `\r\n` Windows files (`session.go:224-234`). Offset written atomically (write-tmp, rename) via `AdvanceOffset()` only on 202.
- **Durable queue** — `internal/queue/queue.go`: JSON array flushed atomically to `queue.json` after every enqueue/dequeue/attempt-count update. Loaded on startup before watcher runs (`main.go:75-79`).
- **Auth — browser sign-in flow** — `internal/auth/auth.go:SignIn()`: ephemeral `127.0.0.1:{random-port}` loopback listener, opens `{coreURL}/auth/signin?redirect_uri=...` in system browser, receives opaque token on callback, stores via `keychain.Store`.
- **Auth — keychain chain store** — `internal/keychain/keychain.go:chainStore`: OS keychain (`zalando/go-keyring`) → DPAPI-encrypted file fallback on Windows (`fallback_windows.go`, `crypt32.dll` via syscall) → in-memory only on non-Windows without Secret Service. Three distinct outcomes: `nil` (OS keychain), `ErrSignedInDegraded` (DPAPI fallback used), `ErrSignedInNotPersisted` (in-memory only). Windows Credential Manager full → DPAPI fallback path confirmed shipped.
- **Auth — opaque token → short-lived JWT** — `internal/auth/auth.go:fetchJWT()`: `POST {coreURL}/v1/auth/token` with `Authorization: Bearer <opaque>`, caches JWT + expiry, single-flight via `refreshMu`, proactive refresh 10 s before expiry. 401 from Core clears keychain and returns `ErrTokenRevoked`.
- **Auth — sign out** — `internal/auth/auth.go:SignOut()`: best-effort `DELETE /v1/auth/token`, clears in-memory JWT + opaque token, deletes keychain entry.
- **Upload — gzip + streaming multipart POST** — `internal/uploader/uploader.go:doUpload()`: pipes file through `gzip.Writer` into multipart part without buffering full payload in memory. Three attempts with delays [0, 200 ms, 400 ms] on transient errors; permanent 4xx returns immediately.
- **Upload response handling** — `internal/uploader/uploader.go`: 202 → returns `job_id`; 401 → `ErrUnauthorized`; 413 → `ErrFileTooLarge`; 429 → `ErrLimitReached`; 400 → `ErrBadRequest{Reason}`; 5xx/network → retried then `ErrTransient`.
- **Upload worker — post-202 cleanup** — `cmd/ludotrace/main.go:runUploadWorker()`: on 202, calls `ext.AdvanceOffset(item.EndOffset)`, `os.Remove(item.TmpPath)`, `q.Remove(item.TmpPath)` in that order.
- **Tray — 6 states** — `internal/tray/tray.go`: `StateIdle`, `StateUploading`, `StateError`, `StateLimitReached`, `StateNotAuth`, `StateNoGames`. Icons embedded via `//go:embed` (`icon.ico`, `icon_error.ico`, `icon_uploading.ico`). Menu items pre-created and show/hidden per state (systray does not support dynamic rebuild).
- **Tray — Open Dashboard** — `internal/tray/tray.go:clickLoop()`: opens `t.appURL` in system browser via `open`/`cmd /c start`/`xdg-open`.
- **Tray — Manage Games** — `internal/tray/tray.go:clickLoop()`: opens `config.FilePath()` in OS default app (same `openBrowser` helper).
- **Tray — Sign In / Sign Out** — `internal/tray/tray.go:clickLoop()`: `auth.SignIn(ctx)` / `auth.SignOut(ctx)` wired; degraded/not-persisted states surfaced to user.
- **Limit reached (429)** — `cmd/ludotrace/main.go:runUploadWorker()`: sets `StateLimitReached`, increments attempts, waits 60 s before retry.
- **Auto-update — client mechanism** — `internal/version/version.go`: `Version` injected via ldflags. `internal/updater/updater.go`: `Check()` fetches the manifest, compares semver, returns an `*Update` only when strictly newer and skips entirely on dev/dirty/untagged builds; `Stage()` streams + SHA-256-verifies + atomically stages `pending_update[.exe]` and writes a `pending_update.json` version sidecar. `cmd/ludotrace/main.go`: `--check-update` one-shot; `--finish-update` self-install; `runUpdateWorker` checks on startup + server-controlled interval; startup is version-aware — re-surfaces "Restart to Update" only when the staged version is newer, else clears the stale leftover (fixes the Windows self-delete-fails → false re-prompt). `internal/tray/tray.go`: `NotifyUpdateReady()` shows real version + "Restart to Update". Validated end-to-end on Windows via `scripts/test-autoupdate.sh` (download / skip-on-dev / apply / stale-cleanup, 9/9) and `go test ./internal/updater` (httptest-driven Check/Stage/reconcile).

---

## Implemented, not yet validated

Code exists and is wired up; not confirmed working under real conditions.

- **Auto-update — release pipeline** — `.github/workflows/release.yml`: on `v*` tag, builds all 4 platforms, computes sha256, creates a GitHub Release, checks out the site repo, copies binaries to `downloads/`, writes `client/version.json`, commits and pushes. Never run — no tagged release yet. `SITE_DEPLOY_TOKEN` is now configured on the repo; `ludotrace.com` resolves to GitHub Pages (apex CNAME → `ludotrace.github.io`, DNS-only). The live manifest at `https://ludotrace.com/client/version.json` does not exist until the first release runs.
- **Orphan / inactivity flush** — `internal/session/session.go:175-179`: if the events file `modTime` is more than `orphanThreshold` (10 minutes — code is authoritative; SPEC.md says 30 min) old and a session is open, it is flushed and enqueued. Correct by inspection but never observed triggering in the wild.
- **Graceful shutdown** — `cmd/ludotrace/main.go:88-131`: `signal.NotifyContext` for `SIGINT`/`SIGTERM`; ctx cancellation stops the watcher (`watcher.go:Start()` returns on `ctx.Done()`), upload worker exits its loop, and a goroutine calls `t.Quit()` to unblock `systray.Run`. The in-flight upload is not explicitly waited on — the worker loop exits on `ctx.Done()` between retries, not after the current HTTP call completes. Not stress-tested; behavior under kill-during-upload is unverified.
- **Transient failure backoff in upload worker** — `cmd/ludotrace/main.go:227-234`: on `ErrTransient` the worker sets `StateError` and waits 30 s. The SPEC describes a distinct calm `StateQueued` state ("N sessions queued — retrying in Xm") for transient failures; that state does not exist in the tray. Transient failures fall through to `StateError` + Retry button, which is not the intended UX but is functional.

---

## Designed (in SPEC), not yet implemented

Appears in `SPEC.md` or `CLAUDE.md` design notes; no implementation exists.

- **Add Game dialog** — `internal/tray/tray.go:286,289`: both `addGame` and `addGameNG` click handlers log `"Add Game clicked — dialog not yet implemented"` and return. `config.AppendGame()` (`internal/config/config.go:87`) and `config.Save()` (`internal/config/config.go:68`) are implemented and ready; only the dialog UI is missing. SPEC.md suggests `sqweek/dialog` for MVP.
- **Auto-start on login** — no code. No launchd plist, Windows Task Scheduler registration, or systemd unit generation anywhere in the repo. CLAUDE.md lists it as out of scope for MVP.
- **StateQueued (offline/retrying) tray state** — `SPEC.md` → System Tray defines a distinct calm `StateQueued` state showing "N sessions queued — retrying in Xm" with a "Retry Now" action, explicitly to distinguish "waiting for connection" from "something is broken". Not present in `internal/tray/tray.go`; transient failures currently collapse into `StateError`.
- **Cloudflare Access service token on client requests** — `internal/features.md` Security section: add `CF-Access-Client-Id` / `CF-Access-Client-Secret` headers to all client HTTP requests to Core. No header injection present in `internal/uploader/uploader.go` or `internal/auth/auth.go`.
