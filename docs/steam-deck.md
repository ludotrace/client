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
| Sign In | **Do this first, from Desktop Mode** — `--sign-in`, needs a browser |
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

## 2. Open the wallet, then sign in

Sign-in needs a browser and a tray button, so it happens in Desktop Mode. The
daemon in Gaming Mode then has to read that same token back — and on the Deck's
default settings it cannot, for a reason worth fixing before you go any further.

**On Linux the token is only ever in the keyring.** Unlike Windows there is no
encrypted on-disk fallback: the client deliberately refuses to write a token as
plaintext (`internal/keychain/fallback_other.go`). So the token is readable only
where the keyring is both *reachable* and *unlocked*.

Reachable is fine. SteamOS 3.8 ships Plasma 6, whose Secret Service provider is
`ksecretd` (KWallet has served `org.freedesktop.secrets` since KDE Frameworks
5.97). It is D-Bus activated, so it does not need a Plasma session running — a
session bus is enough, and Gaming Mode has one. The well-known
"[no keyring on SteamOS][valve928]" complaint is about *gnome-keyring*
specifically; KDE's own provider is present.

**Unlocked is the problem.** The Deck auto-logs-in, so no password is ever
typed, so `kwallet-pam` has nothing to unlock the wallet with. A
password-protected wallet can then only be opened by someone typing the password
into a prompt — which is exactly what Gaming Mode has nobody to do.

So do one of these in Desktop Mode **before** enabling the service:

- **Give the wallet a blank password** (System Settings → KDE Wallet → change
  password, leave it empty). An empty-password wallet opens without prompting,
  in any session. This is the usual Deck workaround and the one that survives a
  mode switch.
- **Or disable auto-login**, so `kwallet-pam` can unlock the wallet with the
  password you type at boot.

While you are in System Settings → KDE Wallet, confirm **"Use KWallet for the
Secret Service interface"** is enabled.

> A blank-password wallet is protected at rest by file permissions rather than a
> passphrase (`~/.local/share/kwalletd/*.kwl`, mode 0600). That is a real
> trade-off, and close to the protection the client declines to implement itself
> — the difference being that it is the OS's store, under the user's control, not
> a token this app wrote in the clear. If that trade is unacceptable, keep the
> wallet password and accept that uploads only run in Desktop Mode.

Then sign in. The headless build has no tray, so this is a one-shot command
rather than a menu item — it opens your browser and waits for you to finish:

```bash
~/.local/bin/ludotrace-linux-headless --sign-in
```

`signed in; token stored in the OS keychain` is what you want. If it instead
says the token **could not be saved**, the wallet is still locked — fix that
above and run it again, because a token that was not written is gone the moment
the command exits.

Verify the token is readable the way the service will read it — a plain
`busctl` presence check is not enough, because a *locked* wallet still answers:

```bash
secret-tool lookup service ludotrace username opaque_token
```

Printing the token means Gaming Mode will be able to read it too. An error, an
empty result, or a password prompt means it will not.

`secret-tool` comes from `libsecret` and may not be present on a stock SteamOS
image, which is read-only by default. If it is missing, skip it — the real test
is step 6: start the service in Gaming Mode and read the journal. A repeating
`failed to get auth token` with a `keychain:` error in it is the locked-wallet
case.

[valve928]: https://github.com/ValveSoftware/SteamOS/issues/928

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
quiet rather than retrying. Sign in from Desktop Mode (step 1).

**Token unreadable — `failed to get auth token`, repeating every 5 seconds.**
Sign-in *did* work in Desktop Mode, but the keyring holding the token is not
reachable from the session the unit runs in. The client cannot tell this apart
from a transient fault, so it retries indefinitely instead of waiting:

```json
{"level":"WARN","msg":"failed to get auth token",
 "err":"auth: load opaque token: keychain: load: ..."}
```

A `keychain:` error inside `err` is the giveaway — a plain "not signed in" is the
case above. Expect roughly 17k of these a day while anything is queued;
`ludotrace.log` rotates at 5 MiB so it will not fill the disk, but it will bury
everything else.

Nearly always this means **the wallet is locked**, not that the token is bad or
missing — see step 1. The Deck auto-logs-in, so nothing unlocks a
password-protected wallet, and Gaming Mode has nobody to answer the prompt.

A provider being present is not the same as the wallet being open, so check
readability rather than presence:

```bash
busctl --user list | grep -i secrets   # is ksecretd reachable at all?
secret-tool lookup service ludotrace username opaque_token   # is it actually readable?
```

A locked wallet answers the first and fails the second. That is the case to fix:
give the wallet a blank password in Desktop Mode, or disable auto-login (step 1).

If the first command lists nothing either, no Secret Service is running in that
session — confirm "Use KWallet for the Secret Service interface" is enabled in
System Settings → KDE Wallet.

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
