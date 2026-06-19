// Package keychain stores and retrieves the single long-lived opaque token
// used to obtain short-lived JWTs from Core. It never touches JWTs directly.
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

// Store is the interface for OS-native secure token storage.
type Store interface {
	// Save persists the opaque token, replacing any existing value.
	Save(token string) error
	// Load retrieves the stored opaque token. Returns ErrNotFound if absent.
	Load() (string, error)
	// Delete removes the stored token. A no-op if already absent.
	Delete() error
}

type osStore struct{}

// New returns a Store backed by the OS-native keychain
// (macOS Keychain, Windows Credential Manager, Linux Secret Service).
func New() Store {
	return &osStore{}
}

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
