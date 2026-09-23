//go:build linux

package keychain

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// hasUserCreds reports whether this machine can seal a user credential.
// systemd-creds gained --user in systemd 256, and CI runners are older, so the
// round-trip tests skip rather than fail where it is unavailable.
func hasUserCreds(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("systemd-creds"); err != nil {
		t.Skip("systemd-creds not installed")
	}
	cmd := exec.Command("systemd-creds", "--user", "--name=lt-probe", "encrypt", "-", "-")
	cmd.Stdin = nil
	if err := cmd.Run(); err != nil {
		t.Skip("systemd-creds --user unavailable here (needs systemd 256+ and a user session)")
	}
}

func TestSystemdCredsRoundTrip(t *testing.T) {
	hasUserCreds(t)

	path := filepath.Join(t.TempDir(), "cred")
	s := newFallbackStore(path)

	if _, err := s.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load before Save = %v, want ErrNotFound", err)
	}
	if err := s.Save("opaque-token-value"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The blob on disk must not contain the token: the whole point is that the
	// key lives with the OS, not beside the ciphertext.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sealed file: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("sealed file is empty")
	}
	if string(raw) == "opaque-token-value" {
		t.Fatal("token was written in the clear")
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("sealed file mode = %o, want 600", perm)
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != "opaque-token-value" {
		t.Fatalf("Load = %q, want the saved token", got)
	}

	if err := s.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load after Delete = %v, want ErrNotFound", err)
	}
}

// Delete is specified as a no-op when nothing is stored, so sign-out on a
// never-signed-in install does not surface an error.
func TestSystemdCredsDeleteAbsentIsNoOp(t *testing.T) {
	s := newFallbackStore(filepath.Join(t.TempDir(), "missing"))
	if err := s.Delete(); err != nil {
		t.Fatalf("Delete on absent file: %v", err)
	}
}

// A missing systemd-creds must report errNoSecureFallback — the same state as a
// platform with no store — rather than falling back to anything weaker.
func TestSystemdCredsUnavailableRefusesToSave(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // hide systemd-creds

	err := newFallbackStore(filepath.Join(t.TempDir(), "cred")).Save("tok")
	if !errors.Is(err, errNoSecureFallback) {
		t.Fatalf("Save without systemd-creds = %v, want errNoSecureFallback", err)
	}
}
