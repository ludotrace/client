//go:build linux

package keychain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Linux has no DPAPI, so this store borrows systemd's: `systemd-creds --user`
// seals the token against a key the OS holds (TPM2-backed where present) and
// hands back a blob that is useless without it. That keeps the rule the other
// platforms' fallbacks keep — the key never lives beside the ciphertext, in the
// binary, or in the file — without inventing a scheme of our own.
//
// It exists because the Secret Service is not a given. A Steam Deck has no
// provider at all: `org.freedesktop.secrets` is not even activatable, so
// keyring.Set fails outright and, before this, sign-in survived only until the
// process exited. That is unworkable for a daemon a service manager restarts.
//
// --user, specifically. `--with-key=tpm2` addresses the TPM directly and is
// refused without interactive authorization
// (io.systemd.InteractiveAuthenticationRequired), which a background service can
// never satisfy. User scope needs no root, no tss group membership, and no
// polkit prompt.
//
// Unavailable means unavailable: no systemd-creds, or a systemd too old for
// --user (pre-256), reports the same errNoSecureFallback as a platform with no
// store at all. It never degrades to writing the token in the clear.
const (
	// credentialName is embedded in the sealed blob and authenticated on
	// decrypt — systemd-creds refuses a blob whose name does not match, so this
	// must stay stable across versions.
	credentialName = "ludotrace-token"

	// credsTimeout bounds each systemd-creds call. TPM operations are quick but
	// not instant, and a wedged helper must not hang the daemon's auth path.
	credsTimeout = 15 * time.Second
)

func newFallbackStore(path string) Store {
	return &systemdCredsStore{path: path}
}

type systemdCredsStore struct {
	path string
}

// run executes systemd-creds with the given args, feeding it stdin and
// returning stdout. stderr is folded into the error: systemd-creds explains
// refusals there, and that text is the difference between a usable report and
// "exit status 1".
func (s *systemdCredsStore) run(stdin []byte, op string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath("systemd-creds"); err != nil {
		return nil, errNoSecureFallback
	}

	ctx, cancel := context.WithTimeout(context.Background(), credsTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "systemd-creds", args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("keychain: systemd-creds timed out after %s", credsTimeout)
		}
		return nil, fmt.Errorf("keychain: systemd-creds %s: %w: %s",
			op, err, bytes.TrimSpace(errBuf.Bytes()))
	}
	return out.Bytes(), nil
}

func (s *systemdCredsStore) Save(token string) error {
	sealed, err := s.run([]byte(token), "encrypt", "--user", "--name="+credentialName, "encrypt", "-", "-")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("keychain: fallback mkdir: %w", err)
	}
	// Written via a temp file and renamed so a crash mid-write cannot leave a
	// truncated blob that would fail to decrypt and read as "signed out".
	// 0600 is defence in depth; the sealing is the real protection.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".ludotrace-cred-*")
	if err != nil {
		return fmt.Errorf("keychain: fallback temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename succeeds

	if _, err := tmp.Write(sealed); err != nil {
		tmp.Close()
		return fmt.Errorf("keychain: fallback write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("keychain: fallback close: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("keychain: fallback chmod: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("keychain: fallback rename: %w", err)
	}
	return nil
}

func (s *systemdCredsStore) Load() (string, error) {
	if _, err := os.Stat(s.path); errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	plain, err := s.run(nil, "decrypt", "--user", "--name="+credentialName, "decrypt", s.path, "-")
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (s *systemdCredsStore) Delete() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("keychain: fallback delete: %w", err)
	}
	return nil
}
