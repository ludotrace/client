//go:build !windows && !linux

package keychain

// newFallbackStore returns a no-op store on the platforms with neither a
// DPAPI equivalent nor systemd: macOS, whose Keychain is reliable enough not to
// need one, and the BSDs. Linux has its own store (fallback_linux.go); the rule
// this file encodes — never write the token as plaintext — is what that store
// had to satisfy rather than sidestep.
func newFallbackStore(path string) Store {
	return unsupportedFallback{}
}
