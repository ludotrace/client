// Package keychain stores and retrieves the single long-lived opaque token
// used to obtain short-lived JWTs from Core. It never touches JWTs directly.
//
// The primary backend is the OS-native keychain (macOS Keychain, Windows
// Credential Manager, Linux Secret Service) via zalando/go-keyring. Because
// that backend can be unavailable — a full Windows Credential Manager rejects
// all writes with ERROR_NOT_ENOUGH_MEMORY, and headless Linux often has no
// Secret Service — New returns a chainStore that falls back to a secure
// on-disk file (DPAPI-encrypted on Windows) so a sign-in survives a restart.
package keychain

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

const (
	service = "ludotrace"
	account = "opaque_token"
)

// ErrNotFound is returned by Load when no token has been stored yet.
var ErrNotFound = errors.New("keychain: token not found")

// ErrUsedFallback is returned (wrapped) by Save when the OS keychain rejected
// the write but the token was persisted to the secure on-disk fallback instead.
// The token IS durable; callers should treat this as a degraded success and
// surface that the OS keychain is unavailable.
var ErrUsedFallback = errors.New("keychain: OS keychain unavailable, token saved to encrypted fallback")

// Store is the interface for secure token storage.
type Store interface {
	// Save persists the opaque token, replacing any existing value.
	Save(token string) error
	// Load retrieves the stored opaque token. Returns ErrNotFound if absent.
	Load() (string, error)
	// Delete removes the stored token. A no-op if already absent.
	Delete() error
}

// New returns a Store backed by the OS-native keychain, falling back to a
// secure on-disk file at fallbackPath when the keychain write fails.
func New(fallbackPath string) Store {
	return &chainStore{
		primary:  &osStore{},
		fallback: newFallbackStore(fallbackPath),
	}
}

// chainStore tries the OS keychain first and falls back to a secure file store.
type chainStore struct {
	primary  Store
	fallback Store
}

func (c *chainStore) Save(token string) error {
	perr := c.primary.Save(token)
	if perr == nil {
		// Primary owns the token now; drop any stale fallback so it can't
		// shadow a future Load with an old value.
		_ = c.fallback.Delete()
		return nil
	}
	if ferr := c.fallback.Save(token); ferr != nil {
		return fmt.Errorf("keychain: save failed (%v) and fallback failed: %w", perr, ferr)
	}
	return fmt.Errorf("%w: %v", ErrUsedFallback, perr)
}

func (c *chainStore) Load() (string, error) {
	tok, err := c.primary.Load()
	if err == nil {
		return tok, nil
	}
	// On any primary failure (absent, or keychain unavailable), consult the
	// fallback before giving up.
	ftok, ferr := c.fallback.Load()
	if ferr == nil {
		return ftok, nil
	}
	if errors.Is(err, ErrNotFound) && errors.Is(ferr, ErrNotFound) {
		return "", ErrNotFound
	}
	// Prefer reporting the primary error; it explains why the keychain failed.
	return "", err
}

func (c *chainStore) Delete() error {
	perr := c.primary.Delete()
	ferr := c.fallback.Delete()
	if perr != nil {
		return perr
	}
	return ferr
}

type osStore struct{}

func (s *osStore) Save(token string) error {
	if err := keyring.Set(service, account, token); err != nil {
		return fmt.Errorf("keychain: save: %w", err)
	}
	return nil
}

func (s *osStore) Load() (string, error) {
	token, err := keyring.Get(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("keychain: load: %w", err)
	}
	return token, nil
}

func (s *osStore) Delete() error {
	err := keyring.Delete(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("keychain: delete: %w", err)
	}
	return nil
}
