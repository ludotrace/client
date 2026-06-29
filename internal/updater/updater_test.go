package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ludotrace/client/internal/version"
)

func TestParseSemver(t *testing.T) {
	cases := []struct {
		input   string
		want    semver
		wantErr bool
	}{
		{"1.2.3", semver{1, 2, 3}, false},
		{"v1.2.3", semver{1, 2, 3}, false},
		// dirty / untagged builds — must be rejected
		{"v1.2.3-4-gabcdef", semver{}, true},
		{"dev", semver{}, true},
		{"1.2.3-beta.1", semver{}, true},
		// malformed
		{"1.2", semver{}, true},
		{"1.2.x", semver{}, true},
	}
	for _, c := range cases {
		got, err := parseSemver(c.input)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSemver(%q): want error, got nil", c.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSemver(%q): unexpected error: %v", c.input, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseSemver(%q) = %+v, want %+v", c.input, got, c.want)
		}
	}
}

func TestNewerThan(t *testing.T) {
	cases := []struct {
		a, b semver
		want bool
	}{
		{semver{1, 3, 0}, semver{1, 2, 0}, true},
		{semver{2, 0, 0}, semver{1, 9, 9}, true},
		{semver{1, 2, 4}, semver{1, 2, 3}, true},
		{semver{1, 2, 3}, semver{1, 2, 3}, false},
		{semver{1, 2, 2}, semver{1, 2, 3}, false},
		{semver{0, 9, 9}, semver{1, 0, 0}, false},
	}
	for _, c := range cases {
		if got := newerThan(c.a, c.b); got != c.want {
			t.Errorf("newerThan(%+v, %+v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v1.3.0", "v1.2.0", true},
		{"1.3.0", "1.2.0", true},
		{"v99.0.0", "v99.0.0", false},      // equal — the already-applied case
		{"v1.0.0", "v1.0.1", false},        // older
		{"v99.0.0", "dev", false},          // running a dev build → never auto-prompt
		{"v1.0.0-3-gabcdef", "v1.0.0", false}, // staged is a dirty build
	}
	for _, c := range cases {
		if got := IsNewer(c.a, c.b); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestStageWritesSidecarAndReconcile drives the full Stage → StagedVersion →
// IsNewer → ClearStaged cycle against an httptest server. This is the portable,
// deterministic core of the auto-update validation (no OS-specific exec needed).
func TestStageWritesSidecarAndReconcile(t *testing.T) {
	binary := []byte("fake new client binary")
	sum := sha256.Sum256(binary)
	wantSHA := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(binary)
	}))
	defer srv.Close()

	dir := t.TempDir()
	u := New(dir)

	// Stage a v99.0.0 update.
	upd := &Update{Version: "99.0.0", DownloadURL: srv.URL + "/bin", SHA256: wantSHA}
	pendingPath, err := u.Stage(context.Background(), upd)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	// Staged binary must match what was served.
	got, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatalf("read staged binary: %v", err)
	}
	if string(got) != string(binary) {
		t.Fatalf("staged binary content mismatch")
	}

	// Sidecar must record the staged version.
	ver, ok := StagedVersion(dir)
	if !ok || ver != "99.0.0" {
		t.Fatalf("StagedVersion = (%q, %v), want (99.0.0, true)", ver, ok)
	}

	// Genuine pending update (newer than a hypothetical running v1.0.0).
	if !IsNewer(ver, "v1.0.0") {
		t.Fatalf("IsNewer(%q, v1.0.0) = false, want true", ver)
	}

	// After "applying" (running version now equals staged), it is stale.
	if IsNewer(ver, "v99.0.0") {
		t.Fatalf("IsNewer(%q, v99.0.0) = true, want false (already applied)", ver)
	}

	// ClearStaged removes both binary and sidecar.
	if err := ClearStaged(dir); err != nil {
		t.Fatalf("ClearStaged: %v", err)
	}
	if _, ok := StagedVersion(dir); ok {
		t.Fatalf("sidecar still present after ClearStaged")
	}
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("pending binary still present after ClearStaged")
	}
	// Clearing again is not an error (idempotent).
	if err := ClearStaged(dir); err != nil {
		t.Fatalf("ClearStaged (second call): %v", err)
	}
}

// TestStageRejectsBadChecksum ensures a corrupted download is not staged.
func TestStageRejectsBadChecksum(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("corrupted bytes"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	u := New(dir)
	upd := &Update{Version: "99.0.0", DownloadURL: srv.URL + "/bin", SHA256: "deadbeef"}

	if _, err := u.Stage(context.Background(), upd); err == nil {
		t.Fatal("Stage accepted a binary with a mismatched checksum")
	}
	if _, ok := StagedVersion(dir); ok {
		t.Fatal("sidecar written despite checksum failure")
	}
	if _, err := os.Stat(PendingPath(dir)); !os.IsNotExist(err) {
		t.Fatal("pending binary present despite checksum failure")
	}
}

// TestCheckSkipsDevBuild verifies a dev/untagged running build never returns an
// update even when the manifest advertises a newer version.
func TestCheckSkipsDevBuild(t *testing.T) {
	// version.Version defaults to "dev" in tests (no ldflags), so Check should
	// short-circuit on the version gate regardless of manifest content.
	manifest := fmt.Sprintf(`{"version":"99.0.0","next_check_seconds":3600,"platforms":{%q:{"url":"http://example/bin","sha256":"abc"}}}`,
		runtime.GOOS+"/"+runtime.GOARCH)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(manifest))
	}))
	defer srv.Close()

	t.Setenv("LUDOTRACE_MANIFEST_URL", srv.URL)
	u := New(t.TempDir())
	upd, _, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if upd != nil {
		t.Fatalf("Check returned an update for a dev build: %+v", upd)
	}
}

// TestCheckFindsNewerRelease verifies a tagged running build is offered a newer
// manifest version. The running version is injected via the package var.
func TestCheckFindsNewerRelease(t *testing.T) {
	orig := version.Version
	version.Version = "v1.0.0"
	defer func() { version.Version = orig }()

	manifest := fmt.Sprintf(`{"version":"1.3.0","next_check_seconds":7200,"platforms":{%q:{"url":"http://example/bin","sha256":"abc"}}}`,
		runtime.GOOS+"/"+runtime.GOARCH)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(manifest))
	}))
	defer srv.Close()

	t.Setenv("LUDOTRACE_MANIFEST_URL", srv.URL)
	u := New(t.TempDir())
	upd, next, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if upd == nil || upd.Version != "1.3.0" {
		t.Fatalf("Check = %+v, want version 1.3.0", upd)
	}
	if next.Seconds() != 7200 {
		t.Fatalf("next check = %v, want 7200s (from manifest)", next)
	}
}

// sanity: ensure JSON sidecar shape is what we expect.
func TestPendingMetaShape(t *testing.T) {
	b, _ := json.Marshal(pendingMeta{Version: "1.2.3"})
	if string(b) != `{"version":"1.2.3"}` {
		t.Fatalf("pendingMeta JSON = %s", b)
	}
	want := filepath.Join("x", "pending_update.json")
	if got := PendingMetaPath("x"); got != want {
		t.Fatalf("PendingMetaPath = %q, want %q", got, want)
	}
}
