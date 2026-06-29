// Package updater handles version checking and staged binary downloads.
// The client calls Check on startup and every N hours; if an update is
// available it calls Stage, then notifies the tray.
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ludotrace/client/internal/version"
)

// ManifestURL is injected at build time via -ldflags for release builds.
// Dev builds leave it empty and skip the version check.
var ManifestURL = ""

const defaultCheckInterval = 24 * time.Hour

// manifest is the JSON structure served at ManifestURL.
type manifest struct {
	Version          string              `json:"version"`
	NextCheckSeconds int                 `json:"next_check_seconds"`
	Platforms        map[string]platform `json:"platforms"`
}

type platform struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Update holds the information needed to download and apply a new version.
type Update struct {
	Version     string
	DownloadURL string
	SHA256      string
}

// Updater checks for updates and stages the downloaded binary.
type Updater struct {
	manifestURL string
	configDir   string
	httpClient  *http.Client
}

// New creates an Updater. configDir is the LudoTrace config directory
// (e.g. %APPDATA%\ludotrace on Windows).
// LUDOTRACE_MANIFEST_URL overrides the compiled-in ManifestURL for testing.
func New(configDir string) *Updater {
	url := ManifestURL
	if override := os.Getenv("LUDOTRACE_MANIFEST_URL"); override != "" {
		url = override
	}
	return &Updater{
		manifestURL: url,
		configDir:   configDir,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Check fetches the manifest, compares the running version against it, and
// returns a non-nil *Update if a newer version is available.
// Returns (nil, defaultCheckInterval, nil) when already up to date or running
// an untagged build.
func (u *Updater) Check(ctx context.Context) (*Update, time.Duration, error) {
	if u.manifestURL == "" {
		slog.Debug("updater: no manifest URL configured, skipping version check")
		return nil, defaultCheckInterval, nil
	}

	current, err := parseSemver(version.Version)
	if err != nil {
		slog.Debug("updater: untagged build, skipping version check", "version", version.Version)
		return nil, defaultCheckInterval, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.manifestURL, nil)
	if err != nil {
		return nil, defaultCheckInterval, fmt.Errorf("build request: %w", err)
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, defaultCheckInterval, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, defaultCheckInterval, fmt.Errorf("manifest returned HTTP %d", resp.StatusCode)
	}

	var m manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, defaultCheckInterval, fmt.Errorf("decode manifest: %w", err)
	}

	interval := defaultCheckInterval
	if m.NextCheckSeconds > 0 {
		interval = time.Duration(m.NextCheckSeconds) * time.Second
	}

	latest, err := parseSemver(m.Version)
	if err != nil {
		return nil, interval, fmt.Errorf("parse manifest version %q: %w", m.Version, err)
	}

	if !newerThan(latest, current) {
		slog.Debug("updater: already up to date", "version", version.Version)
		return nil, interval, nil
	}

	key := runtime.GOOS + "/" + runtime.GOARCH
	p, ok := m.Platforms[key]
	if !ok {
		slog.Debug("updater: no binary for platform, skipping", "platform", key)
		return nil, interval, nil
	}

	slog.Info("updater: newer version available", "current", version.Version, "latest", m.Version)
	return &Update{
		Version:     m.Version,
		DownloadURL: p.URL,
		SHA256:      p.SHA256,
	}, interval, nil
}

// Stage downloads the update binary, verifies its SHA-256, and atomically
// renames it to <configDir>/pending_update[.exe].
// Returns the path to the staged binary.
func (u *Updater) Stage(ctx context.Context, upd *Update) (string, error) {
	pendingPath := PendingPath(u.configDir)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upd.DownloadURL, nil)
	if err != nil {
		return "", fmt.Errorf("build download request: %w", err)
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download binary: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	// Write to a temp file in the same directory so the rename is atomic.
	tmpFile, err := os.CreateTemp(u.configDir, "update-download-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmpFile, h), resp.Body); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("write download: %w", err)
	}
	tmpFile.Close()

	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, upd.SHA256) {
		os.Remove(tmpPath)
		return "", fmt.Errorf("sha256 mismatch: got %s, want %s", got, upd.SHA256)
	}

	// Make the staged binary executable before renaming.
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("chmod staged binary: %w", err)
	}

	if err := os.Rename(tmpPath, pendingPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("stage binary: %w", err)
	}

	// Record the staged version in a sidecar so startup can tell a genuine
	// pending update apart from a stale binary left over after a prior apply
	// (the running process cannot delete itself on Windows; see ClearStaged).
	meta, err := json.Marshal(pendingMeta{Version: upd.Version})
	if err != nil {
		return "", fmt.Errorf("marshal pending meta: %w", err)
	}
	if err := os.WriteFile(PendingMetaPath(u.configDir), meta, 0o600); err != nil {
		return "", fmt.Errorf("write pending meta: %w", err)
	}

	slog.Info("updater: update staged", "version", upd.Version, "path", pendingPath)
	return pendingPath, nil
}

// pendingMeta is the JSON sidecar recording the staged binary's version.
type pendingMeta struct {
	Version string `json:"version"`
}

// PendingPath returns the path where a staged update binary is stored.
func PendingPath(configDir string) string {
	name := "pending_update"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(configDir, name)
}

// PendingMetaPath returns the path of the staged update's version sidecar.
func PendingMetaPath(configDir string) string {
	return filepath.Join(configDir, "pending_update.json")
}

// StagedVersion reports the version of a currently-staged update, reading the
// sidecar written by Stage. ok is false when no readable sidecar exists.
func StagedVersion(configDir string) (version string, ok bool) {
	data, err := os.ReadFile(PendingMetaPath(configDir))
	if err != nil {
		return "", false
	}
	var meta pendingMeta
	if err := json.Unmarshal(data, &meta); err != nil || meta.Version == "" {
		return "", false
	}
	return meta.Version, true
}

// ClearStaged removes the staged binary and its sidecar. Missing files are not
// an error. Used to drop a stale staged update after it has been applied — the
// freshly relaunched (install-path) process can delete the pending binary
// because the pending process that wrote it has already exited.
func ClearStaged(configDir string) error {
	if err := os.Remove(PendingPath(configDir)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(PendingMetaPath(configDir)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// IsNewer reports whether release version a is strictly newer than b.
// Returns false if either is not a clean release semver (e.g. "dev"), so a
// dev build never treats a staged release as something to auto-prompt.
func IsNewer(a, b string) bool {
	av, err1 := parseSemver(a)
	bv, err2 := parseSemver(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return newerThan(av, bv)
}

// semver holds a parsed major.minor.patch triple.
type semver struct{ major, minor, patch int }

// parseSemver parses a clean release version string (e.g. "v1.2.3" or "1.2.3").
// Any string containing a "-" after the optional "v" prefix is rejected — this
// covers "dev", dirty git-describe strings ("v1.2.3-4-gabcdef"), and pre-release
// labels, all of which should not be auto-updated.
func parseSemver(s string) (semver, error) {
	s = strings.TrimPrefix(s, "v")
	if strings.Contains(s, "-") {
		return semver{}, fmt.Errorf("not a release build: %q", s)
	}
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("not a semver: %q", s)
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	p, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return semver{}, fmt.Errorf("not a semver: %q", s)
	}
	return semver{major, minor, p}, nil
}

// newerThan returns true if a is strictly greater than b.
func newerThan(a, b semver) bool {
	if a.major != b.major {
		return a.major > b.major
	}
	if a.minor != b.minor {
		return a.minor > b.minor
	}
	return a.patch > b.patch
}
