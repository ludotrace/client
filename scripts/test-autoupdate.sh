#!/usr/bin/env bash
#
# Manual end-to-end validation of the client auto-update flow.
#
# NOT part of CI. This drives the real Windows .exe from WSL because the
# apply path (self-overwrite + relaunch + stale-file cleanup) depends on
# Windows file-locking semantics that a Go test cannot exercise. The
# portable decision logic (version compare, download, SHA-256 verify,
# sidecar reconcile) is covered by `go test ./internal/updater`; run that
# first — this script only adds the OS-specific pieces.
#
# Requirements: WSL with Windows interop, python3, go, a writable
# %APPDATA%\ludotrace. Run from anywhere inside the client repo.
#
# Usage:  ./scripts/test-autoupdate.sh
#
set -uo pipefail

# ── locate repo + config dir ────────────────────────────────────────────────
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

APPDATA_WIN="$(cmd.exe /c "echo %APPDATA%" 2>/dev/null | tr -d '\r')"
CFGDIR="$(wslpath -u "$APPDATA_WIN")/ludotrace"
mkdir -p "$CFGDIR"

PORT=19999
WORK="$REPO/dist/autoupdate-test"
SRV="$WORK/server"
PID_FILE="$WORK/server.pid"

PASS=0
FAIL=0
note()  { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }
ok()    { printf '  \033[32mPASS\033[0m %s\n' "$*"; PASS=$((PASS+1)); }
bad()   { printf '  \033[31mFAIL\033[0m %s\n' "$*"; FAIL=$((FAIL+1)); }

cleanup() {
  [ -f "$PID_FILE" ] && kill "$(cat "$PID_FILE")" 2>/dev/null
  pkill -f "http.server $PORT" 2>/dev/null
  powershell.exe -NoProfile -Command \
    "Stop-Process -Name pending,original,lt-rel,lt-dev -Force -ErrorAction SilentlyContinue" 2>/dev/null
  rm -f "$CFGDIR/ludotrace.lock" "$CFGDIR/pending_update.exe" "$CFGDIR/pending_update.json"
  rm -rf "$WORK"
}
trap cleanup EXIT

run_check() { # <exe> <baked-version>  -> runs --check-update, echoes stdout
  local exe="$1"
  local win; win="$(wslpath -w "$exe")"
  local bat="$WORK/run.bat"
  {
    echo '@echo off'
    echo "set LUDOTRACE_LOG_LEVEL=debug"
    echo "set LUDOTRACE_MANIFEST_URL=http://localhost:$PORT/client/version.json"
    echo "\"$win\" --check-update"
  } > "$bat"
  sed -i 's/$/\r/' "$bat"
  cmd.exe /c "$(wslpath -w "$bat")" 2>&1
}

# ── build console test binaries (no -H=windowsgui, so stdout is visible) ─────
note "Building test binaries"
mkdir -p "$WORK" "$SRV/client" "$SRV/downloads"
build() { # <out> <version>
  CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build \
    -ldflags "-X github.com/ludotrace/client/internal/version.Version=$2" \
    -o "$1" ./cmd/ludotrace
}
build "$WORK/lt-rel.exe" "v1.0.0"  || { echo "build failed"; exit 1; }
build "$WORK/lt-dev.exe" "dev"     || { echo "build failed"; exit 1; }
echo "  built lt-rel.exe (v1.0.0) and lt-dev.exe (dev)"

# ── serve a manifest advertising 99.0.0 with the release binary ─────────────
cp "$WORK/lt-rel.exe" "$SRV/downloads/ludotrace.exe"
SHA="$(sha256sum "$SRV/downloads/ludotrace.exe" | awk '{print $1}')"
cat > "$SRV/client/version.json" <<EOF
{
  "version": "99.0.0",
  "next_check_seconds": 86400,
  "platforms": {
    "windows/amd64": { "url": "http://localhost:$PORT/downloads/ludotrace.exe", "sha256": "$SHA" }
  }
}
EOF
python3 -m http.server "$PORT" --directory "$SRV" >/dev/null 2>&1 &
echo $! > "$PID_FILE"
sleep 1

# ── scenario 1: release build sees newer version → downloads + stages ───────
note "Scenario 1: release build (v1.0.0) vs manifest 99.0.0 — expect download"
rm -f "$CFGDIR/pending_update.exe" "$CFGDIR/pending_update.json"
OUT="$(run_check "$WORK/lt-rel.exe")"
echo "$OUT" | grep -q "update staged" && ok "logged 'update staged'" || bad "no 'update staged' log"
if [ -f "$CFGDIR/pending_update.exe" ]; then ok "pending_update.exe created"; else bad "pending_update.exe missing"; fi
STAGED_SHA="$(sha256sum "$CFGDIR/pending_update.exe" 2>/dev/null | awk '{print $1}')"
[ "$STAGED_SHA" = "$SHA" ] && ok "staged SHA matches served binary" || bad "staged SHA mismatch"
if [ -f "$CFGDIR/pending_update.json" ]; then ok "version sidecar written"; else bad "sidecar missing"; fi

# ── scenario 2: dev build → skips even though manifest URL is set ────────────
note "Scenario 2: dev build vs manifest 99.0.0 — expect SKIP (no download)"
rm -f "$CFGDIR/pending_update.exe" "$CFGDIR/pending_update.json"
OUT="$(run_check "$WORK/lt-dev.exe")"
echo "$OUT" | grep -q "no update available" && ok "logged 'no update available'" || bad "expected skip log missing"
if [ ! -f "$CFGDIR/pending_update.exe" ]; then ok "no pending_update.exe (correct)"; else bad "dev build downloaded an update"; fi

# ── scenario 3: apply via --finish-update overwrites target + relaunches ─────
note "Scenario 3: --finish-update self-install"
cp "$WORK/lt-rel.exe" "$WORK/pending.exe"   # v1.0.0 (the 'new' binary)
cp "$WORK/lt-dev.exe" "$WORK/original.exe"  # 'old' install, distinct bytes
PSHA="$(sha256sum "$WORK/pending.exe" | awk '{print $1}')"
PWIN="$(wslpath -w "$WORK/pending.exe")"; OWIN="$(wslpath -w "$WORK/original.exe")"
bat="$WORK/apply.bat"
{ echo '@echo off'; echo "\"$PWIN\" --finish-update \"$OWIN\""; } > "$bat"
sed -i 's/$/\r/' "$bat"
# relaunched daemon is long-running; cap it and kill afterward
timeout 8 cmd.exe /c "$(wslpath -w "$bat")" >/dev/null 2>&1
sleep 2
powershell.exe -NoProfile -Command "Stop-Process -Name original,pending -Force -ErrorAction SilentlyContinue" 2>/dev/null
OSHA="$(sha256sum "$WORK/original.exe" | awk '{print $1}')"
[ "$OSHA" = "$PSHA" ] && ok "target overwritten with new binary" || bad "target not overwritten"
# self-delete fails on Windows by design; cleanup happens on next startup (scenario 4)
[ -f "$WORK/pending.exe" ] && ok "pending binary survives (Windows lock — expected; cleaned at startup)" \
  || ok "pending binary already removed (Unix-style unlink)"

# ── scenario 4: stale staged update is cleared on next startup ───────────────
note "Scenario 4: stale staged update cleanup on startup"
# Simulate the post-apply state: a staged 1.0.0 sitting next to a running 99.0.0.
cp "$WORK/lt-rel.exe" "$CFGDIR/pending_update.exe"
echo '{"version":"1.0.0"}' > "$CFGDIR/pending_update.json"
rm -f "$CFGDIR/ludotrace.lock"
# Build a 99.0.0 console daemon, start it briefly so its startup reconcile runs.
build "$WORK/lt-99.exe" "v99.0.0"
W99="$(wslpath -w "$WORK/lt-99.exe")"
bat="$WORK/start99.bat"; { echo '@echo off'; echo "\"$W99\""; } > "$bat"; sed -i 's/$/\r/' "$bat"
timeout 6 cmd.exe /c "$(wslpath -w "$bat")" >/dev/null 2>&1
sleep 2
powershell.exe -NoProfile -Command "Stop-Process -Name lt-99 -Force -ErrorAction SilentlyContinue" 2>/dev/null
rm -f "$CFGDIR/ludotrace.lock"
if [ ! -f "$CFGDIR/pending_update.exe" ] && [ ! -f "$CFGDIR/pending_update.json" ]; then
  ok "stale staged update cleared (no false 'Restart to Update' prompt)"
else
  bad "stale staged update left on disk"
fi

# ── summary ─────────────────────────────────────────────────────────────────
note "Summary"
printf '  %d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
