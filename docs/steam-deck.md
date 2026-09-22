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
| Sign In | Desktop Mode, `--sign-in` — needs a browser once |
| Add Game | Not available — write `config.toml` by hand (below) |
| Sign Out | `--sign-out` |
| Auto-update | Downloads and stages, but applies on the next service restart |

**Use the `ludotrace-linux-headless` build.** SteamOS does not ship
`libayatana-appindicator3`, which the normal Linux binary links for its tray, so
that binary cannot start on a Deck at all:

```
ludotrace-linux: error while loading shared libraries:
libayatana-appindicator3.so.1: cannot open shared object file
```

The `--headless` flag does not help, because that link is resolved by the loader
before any code runs. The headless build is compiled without the tray and
without cgo, so it is statically linked and needs nothing installed.

---

## 1. Install the binary

Download **`ludotrace-linux-headless`** from the [latest release][releases], or
from a CI run's `ludotrace-linux-headless` artifact for an untagged build. Not
`ludotrace-linux` — that one will not start here.

```bash
mkdir -p ~/.local/bin
install -m 755 ~/Downloads/ludotrace-linux-headless ~/.local/bin/ludotrace-linux-headless
```

It is static, so there is nothing else to install and nothing to check:

```bash
ldd ~/.local/bin/ludotrace-linux-headless   # "not a dynamic executable"
```

## 2. Sign in

Sign-in needs a browser, so it happens in Desktop Mode. The headless build has
no tray, so it is a one-shot command rather than a menu item:

```bash
~/.local/bin/ludotrace-linux-headless --sign-in
```

Your browser opens; finish there and come back. `signed in; token stored` is
what you want.

### Where the token goes, and why it is not the keyring

SteamOS has **no Secret Service provider at all** — `org.freedesktop.secrets` is
not even activatable, in either mode:

```
keychain: save: The name is not activatable
```

That is not a locked wallet, and no KDE Wallet setting fixes it; there is
nothing installed to unlock. So the client falls back to sealing the token with
`systemd-creds --user`, which encrypts against a key the OS holds — TPM2-backed
on a Deck. The sealed blob lands in `~/.config/ludotrace/`, and the key is never
in the file, the binary, or beside the ciphertext.

That keeps the rule the other platforms keep (Windows uses DPAPI for the same
reason) rather than writing the token in the clear, which the client refuses to
do. `--with-key=tpm2` is deliberately *not* used: addressing the TPM directly
needs interactive authorization a background service can never give, while user
scope needs no root, no `tss` group and no prompt.

Needs systemd 256 or newer; SteamOS 3.8 ships 257. On an older system the client
reports that the token could not be saved rather than persisting it weakly.

If `--sign-in` instead says the token **could not be saved**, check:

```bash
systemd-creds --user --name=t encrypt - - <<< probe >/dev/null && echo "sealing works"
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

### Nothing uploads

Capture is unaffected in both cases below: the watcher still sees writes,
sessions are still extracted, and they queue to disk. Nothing is dropped until
`QUEUE_MAX_AGE_DAYS` (default 30), and the backlog goes out once a token is
readable. These are upload-side failures only.

They look different in the log, and the difference tells you which one you have.

**No token stored — `client state — sign in from a desktop session to resume
uploads`, once, at startup.** Nobody has signed in on this machine. The upload
worker stops and waits for a sign-in, which headless cannot offer, so it goes
quiet rather than retrying. Sign in from Desktop Mode (step 2).

**Token unreadable — `failed to get auth token`, repeating every 5 seconds.**
The token could not be read back. The client cannot tell this apart from a
transient fault, so it retries indefinitely rather than waiting:

```json
{"level":"WARN","msg":"failed to get auth token",
 "err":"auth: load opaque token: keychain: ..."}
```

A `keychain:` error inside `err` is the giveaway — a plain "not signed in" is the
case above. Expect roughly 17k of these a day while anything is queued;
`ludotrace.log` rotates at 5 MiB so it will not fill the disk, but it will bury
everything else.

On a Deck the token is sealed with `systemd-creds --user` (step 2), so check
that sealing still works for your user:

```bash
systemd-creds --user --name=t encrypt - - <<< probe >/dev/null && echo "sealing works"
```

If that fails, so will the client. If it succeeds but the client still cannot
read the token, re-run `--sign-in` — the stored blob may predate a change that
invalidated it, and re-sealing is cheap.

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
rm ~/.local/bin/ludotrace-linux-headless
```

State in `~/.config/ludotrace` (config, queue, offsets) is left in place; delete
that directory too for a clean removal.

[releases]: https://github.com/ludotrace/client/releases/latest
