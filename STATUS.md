# LudoTrace Client — Implementation Status

What is actually built in the background watcher/uploader.

Present-tense state only — no history, no rationale, no test inventories. Use `git log` and
closed issues to find out when or why something changed. See root `AGENTS.md` § STATUS.md
discipline before editing.

---

## Verified end-to-end

Confirmed by manual end-to-end test or observed running.

**Lifecycle**
- Singleton lock — PID-based, reclaims a lock left by a dead process (#26)
- Config load + validation, with env overrides
- Auto-start on login
- Auto-update — client-side check plus the release pipeline that publishes binaries and the
  version manifest
- File logging
- Resource-footprint tests (#13)

**Capture**
- File watcher — fsnotify WRITE events with a 2s debounce
- Startup extraction for every configured game
- Session extraction with sidecar offset tracking, bounded by `session_start`
- Durable queue (`queue.json`) — extraction never uploads directly

**Auth**
- Browser sign-in flow through Core's loopback redirect
- Keychain-backed token store
- Opaque token exchanged for a short-lived Core-signed JWT per upload
- Sign out

**Upload**
- gzip + streaming multipart POST
- Response handling, with the offset advanced on 202
- Post-202 cleanup of the temp session file
- 429 limit-reached handling

**Tray**
- Six states, driven from queue + auth state
- Open Dashboard, Manage Games, Sign In / Sign Out
- Add Game dialog — Steam discovery plus the Core-sourced known-games fallback, validated
  end-to-end with a non-Steam Fallout 4 folder

---

## Implemented, not yet validated

- Bounded upload queue — age-based drop-oldest, newest-first upload (#60)
- Permanent-failure retirement — a 400 or too-large item advances the offset past the rejected region, removes its temp file, and leaves the queue by path (#76)
- Monotonic offset advancement, so newest-first retirement cannot rewind the read position (#76)
- Honors Core's `Retry-After` on 429 (#59)
- Orphan / inactivity flush — closes the final session of a play period
- Graceful shutdown
- Transient-failure backoff and queued state for offline handling
- Launch heartbeat splash (#51)
- W3C traceparent propagation (#64, narrow slice)

---

## Designed, not yet implemented

- Full client-side OpenTelemetry instrumentation — spans, metrics, logs (#66)
- Autostart hardening — warn on a non-permanent path (#24), surface registry-write failure in
  the tray (#23), deregister on uninstall (#22)
- Private vulnerability reporting, before the repo is flipped public (#42)
