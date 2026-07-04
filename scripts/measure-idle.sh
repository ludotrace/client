#!/usr/bin/env bash
#
# Measures the client's resource footprint at idle: peak RSS and goroutine
# count over a run with no games configured. Exits non-zero if either
# exceeds budget.
#
# Budget: 35 MB RSS, 22 goroutines. Measured baseline (client#13) on a
# headless Linux run is ~31 MB / 18 goroutines, of which ~13 goroutines and
# ~30 MB are fixed cost before any of the client's own long-running loops
# start: Go runtime (GC/sysmon/etc, ~7), systray's native event loop (1),
# stdlib signal.NotifyContext internals (2), the D-Bus connection
# getlantern/systray's Linux appindicator backend opens (2), and fsnotify's
# inotify reader (1). The remaining ~5 are the client's own workers
# (watcher, upload, update, tray event loop, shutdown bridge) — already
# consolidated to one goroutine per concern. There is no further headroom to
# cut without dropping functionality, so the limits below track the real
# floor with modest slack rather than an arbitrary target.
#
# Linux only, via /proc/<pid>/status (client#13). Windows RSS measurement is
# a manual `tasklist` check — not automated here.
#
# The tray needs a GTK backend to start. On a headless box, wrap this with
# Xvfb + a D-Bus session, e.g.:
#   xvfb-run -a dbus-run-session -- scripts/measure-idle.sh
#
# Usage:  ./scripts/measure-idle.sh [duration_seconds]
#
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

DURATION="${1:-60}"
SAMPLE_INTERVAL=1
RSS_LIMIT_KB=$((35 * 1024))
GOROUTINE_LIMIT=22

WORK="$(mktemp -d)"
BIN="$WORK/ludotrace-measure"
PID=""

cleanup() {
  if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
    kill -QUIT "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "== building =="
CGO_ENABLED=1 go build -o "$BIN" ./cmd/ludotrace || exit 1

# os.UserConfigDir() reads XDG_CONFIG_HOME on Linux, so pointing it at an
# empty temp dir guarantees "no games configured" without touching the
# developer's real ~/.config/ludotrace, queue, or offsets.
export XDG_CONFIG_HOME="$WORK/config"
mkdir -p "$XDG_CONFIG_HOME"

echo "== launching (no games configured) =="
"$BIN" >"$WORK/stdout.log" 2>"$WORK/stderr.log" &
PID=$!

sleep 1
if ! kill -0 "$PID" 2>/dev/null; then
  echo "FAIL: process exited immediately — check a tray backend (DISPLAY/D-Bus) is available; see stderr:" >&2
  cat "$WORK/stderr.log" >&2
  exit 1
fi

max_rss_kb=0
elapsed=0
while [ "$elapsed" -lt "$DURATION" ]; do
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "FAIL: process exited during measurement window; see stderr:" >&2
    cat "$WORK/stderr.log" >&2
    exit 1
  fi
  rss="$(awk '/^VmRSS:/ {print $2}' "/proc/$PID/status" 2>/dev/null)"
  rss="${rss:-0}"
  if [ "$rss" -gt "$max_rss_kb" ]; then
    max_rss_kb="$rss"
  fi
  sleep "$SAMPLE_INTERVAL"
  elapsed=$((elapsed + SAMPLE_INTERVAL))
done

echo "== capturing goroutine dump (SIGQUIT terminates the process) =="
kill -QUIT "$PID"
wait "$PID" 2>/dev/null || true
goroutines="$(grep -c '^goroutine [0-9]\+ ' "$WORK/stderr.log" || true)"
PID=""

echo "idle RSS (max over ${DURATION}s): $((max_rss_kb / 1024)) MB (${max_rss_kb} KB)"
echo "goroutine count at exit: $goroutines"

status=0
if [ "$max_rss_kb" -gt "$RSS_LIMIT_KB" ]; then
  echo "FAIL: RSS ${max_rss_kb}KB exceeds ${RSS_LIMIT_KB}KB budget" >&2
  status=1
fi
if [ "$goroutines" -gt "$GOROUTINE_LIMIT" ]; then
  echo "FAIL: goroutine count $goroutines exceeds $GOROUTINE_LIMIT budget" >&2
  status=1
fi

exit "$status"
