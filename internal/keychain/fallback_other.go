//go:build !windows

package keychain

import "errors"

// errNoSecureFallback signals that this platform has no secure on-disk fallback.
// We deliberately refuse to write the token as plaintext, so when the OS
// keychain is unavailable here the token lives in memory only for the session
// and the caller surfaces a sign-in-won't-persist warning.
var errNoSecureFallback = errors.New("keychain: no secure on-disk fallback on this platform")

// newFallbackStore returns a no-op store on non-Windows platforms. macOS
// Keychain is reliable; a secure Linux fallback would need a key store we
// cannot assume exists (the very thing that failed), so we do not pretend.
func newFallbackStore(path string) Store {
	return unsupportedFallback{}
}

type unsupportedFallback struct{}

func (unsupportedFallback) Save(string) error     { return errNoSecureFallback }
func (unsupportedFallback) Load() (string, error) { return "", ErrNotFound }
func (unsupportedFallback) Delete() error         { return nil }
