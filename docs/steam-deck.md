# Running LudoTrace on a Steam Deck

Runs the client as a systemd **user** service so it keeps capturing while the Deck
is in Gaming Mode.

SteamOS runs one user session across both Desktop and Gaming Mode, so a user
service enabled once keeps running when you switch modes. What it does *not*
have in Gaming Mode is a desktop: no tray, no file picker, no browser. That
shapes everything below.

---

## What works in Gaming Mode, and what doesn't

| | |
|---|---|
| Watching, extracting, queueing, uploading | Works — this is the whole daemon |
| Tray icon and menu | Not available — use `--headless` |
| Sign In | **Do this first, from Desktop Mode** — it needs a browser |
| Add Game | Not available — write `config.toml` by hand (below) |
| Auto-update | Downloads and stages, but applies on the next service restart |

`--headless` is not a convenience flag. The Linux tray is cgo GTK3, and
`gtk_init` **exits the process** when it cannot open a display:

```
(ludotrace-linux:3811): Gtk-WARNING **: cannot open display:
$ echo $?
1
```

Without the flag the daemon dies at startup having watched nothing, and
`Restart=on-failure` turns that into a crash loop.

---

## 1. Sign in first, from Desktop Mode

Sign-in opens a browser and stores the token in the OS keychain. A headless
daemon can do neither, so it must already be signed in before you enable the
service.

Switch to Desktop Mode, run the binary normally (no `--headless`), and click
**Sign In** in the tray.

> **On Linux the token is only ever in the Secret Service.** Unlike Windows,
> there is no encrypted on-disk fallback — the client deliberately refuses to
> write a token as plaintext. If the Secret Service is not reachable in Gaming
> Mode, the daemon runs but parks signed out. See the troubleshooting section.

## 2. Install the binary

Download `ludotrace-linux` from the [latest release][releases], or from a CI run's
`ludotrace-linux` artifact for an untagged build.

```bash
mkdir -p ~/.local/bin
install -m 755 ~/Downloads/ludotrace-linux ~/.local/bin/ludotrace-linux
```

## 3. Find the Fallout 4 install directory

**The events file lives in the game's install folder, not in `My Games`.** The
Papyrus mod writes with a bare filename, which Hydra resolves against the game
root. `Documents/My Games/Fallout4` inside the Proton prefix holds F4SE and
Hydra's own logs — the events file is never written there, so watching it
captures nothing.

On internal storage that is:

```
~/.local/share/Steam/steamapps/common/Fallout 4
```

If Fallout 4 is on an SD card or another drive, find its library root:

```bash
grep -i '"path"' ~/.local/share/Steam/config/libraryfolders.vdf
```

then look for `<library>/steamapps/common/Fallout 4`. SD cards usually mount
under `/run/media/`. Confirm before continuing — a `watch_path` that does not
exist is **skipped with a warning and nothing is watched**:

```bash
ls -d ~/.local/share/Steam/steamapps/common/"Fallout 4"
```

## 4. Write the config

`config.toml` is not created for you.

```bash
mkdir -p ~/.config/ludotrace
cat > ~/.config/ludotrace/config.toml <<'EOF'
core_url = "https://core.ludotrace.com"

[[games]]
game_id     = "fallout4"
watch_path  = "/home/deck/.local/share/Steam/steamapps/common/Fallout 4"
events_file = "lt_fallout4_events.jsonl"
EOF
```

`watch_path` must be an absolute path — `~` is not expanded, and the service
runs without a shell to expand it. Replace `/home/deck` if your user is not
`deck`, and replace the whole path if the game is on an SD card.

`events_file` is derived from `game_id` (`lt_<game_id>_events.jsonl`) and is
overwritten on load, so it is shown here only to make the config readable. The
name follows `game_id`: get `game_id` wrong and the daemon watches for a
filename the mod never writes.

## 5. Install and enable the service

```bash
mkdir -p ~/.config/systemd/user
curl -fsSL -o ~/.config/systemd/user/ludotrace.service \
  https://raw.githubusercontent.com/ludotrace/client/main/packaging/systemd/ludotrace.service

systemctl --user daemon-reload
systemctl --user enable --now ludotrace.service
```

(Or copy `packaging/systemd/ludotrace.service` from a checkout.)

To keep it running when you are not logged in — the Deck normally auto-logs in,
so this is usually unnecessary:

```bash
sudo loginctl enable-linger $USER
```

## 6. Verify

```bash
systemctl --user status ludotrace.service
journalctl --user -u ludotrace.service -f
```

A healthy start looks like:

```json
{"level":"INFO","msg":"ludotrace client started","games":1,"headless":true}
{"level":"INFO","msg":"tray disabled (headless) — state changes are logged, menu actions unavailable"}
```

`"games":1` is the line that matters — `"games":0` means your `watch_path` was
rejected (step 3).

The client also writes `~/.config/ludotrace/ludotrace.log`, which survives
independently of the journal.

---

## Troubleshooting

Headless runs log every state change that would otherwise have been a tray
icon, so the journal is the status display.

**`"games":0` on startup.** `watch_path` does not exist or is not a directory.
Games are dropped with `config: skipping game with missing or non-directory
watch_path`. Re-check step 3 — quoting the space in `Fallout 4` is the usual
culprit.

**`client state — sign in from a desktop session to resume uploads`.** The
daemon is running but has no usable token, so uploads are parked. Nothing is
lost — sessions keep queueing to disk and go out once you are signed in.

This is also what you see if sign-in *did* work in Desktop Mode but the Secret
Service is not reachable in Gaming Mode: there is no on-disk fallback on Linux,
so the token is simply unreadable. The giveaway is a `keychain: load:` error
rather than a plain "not signed in":

```json
{"level":"WARN","msg":"failed to get auth token",
 "err":"auth: load opaque token: keychain: load: ..."}
```

Check whether a Secret Service is running in the session the unit lives in:

```bash
busctl --user list | grep -i secrets
```

Nothing listed means no provider is running. Switch to Desktop Mode (which runs
KDE's, and therefore has one) to confirm the account is signed in, and treat a
Gaming-Mode-only failure as this and not as a bad token.

Queued sessions are safe throughout — they stay on disk and upload once a token
is readable again.

**`client state — no games configured`.** `config.toml` is missing or has no
`[[games]]` block that survived validation. Add Game is a tray action and is
unreachable here, so this is always a config edit.

**The service restarts repeatedly, then stops.** `StartLimitBurst` gave up after
five failures in five minutes. Read the failure with:

```bash
systemctl --user status ludotrace.service
```

`cannot open display` means `--headless` is missing from `ExecStart`.

**An update was downloaded but the version never changes.** Headless has no
"Restart to Update" to click. The staged binary applies on the next start:

```bash
systemctl --user restart ludotrace.service
```

**Turning up logging.** Add a drop-in rather than editing the unit:

```bash
systemctl --user edit ludotrace.service
```

```ini
[Service]
Environment=LUDOTRACE_LOG_LEVEL=debug
```

---

## Uninstalling

```bash
systemctl --user disable --now ludotrace.service
rm ~/.config/systemd/user/ludotrace.service
systemctl --user daemon-reload
rm ~/.local/bin/ludotrace-linux
```

State in `~/.config/ludotrace` (config, queue, offsets) is left in place; delete
that directory too for a clean removal.

[releases]: https://github.com/ludotrace/client/releases/latest
