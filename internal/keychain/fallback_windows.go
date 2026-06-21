//go:build windows

package keychain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// newFallbackStore returns a DPAPI-encrypted file store. The token is encrypted
// with CryptProtectData (CRYPTPROTECT_LOCAL_MACHINE off → scoped to the current
// Windows user), so the file is useless to other users and never holds plaintext.
func newFallbackStore(path string) Store {
	return &dpapiFileStore{path: path}
}

type dpapiFileStore struct {
	path string
}

func (s *dpapiFileStore) Save(token string) error {
	enc, err := protect([]byte(token))
	if err != nil {
		return fmt.Errorf("keychain: dpapi protect: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("keychain: fallback mkdir: %w", err)
	}
	// 0600: defence in depth; DPAPI is the real protection.
	if err := os.WriteFile(s.path, enc, 0o600); err != nil {
		return fmt.Errorf("keychain: fallback write: %w", err)
	}
	return nil
}

func (s *dpapiFileStore) Load() (string, error) {
	enc, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("keychain: fallback read: %w", err)
	}
	dec, err := unprotect(enc)
	if err != nil {
		return "", fmt.Errorf("keychain: dpapi unprotect: %w", err)
	}
	return string(dec), nil
}

func (s *dpapiFileStore) Delete() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("keychain: fallback delete: %w", err)
	}
	return nil
}

// --- DPAPI bindings (crypt32.dll) ---

const cryptProtectUIForbidden = 0x1

var (
	crypt32            = syscall.NewLazyDLL("crypt32.dll")
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procCryptProtect   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotect = crypt32.NewProc("CryptUnprotectData")
	procLocalFree      = kernel32.NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(b []byte) dataBlob {
	if len(b) == 0 {
		return dataBlob{}
	}
	return dataBlob{cbData: uint32(len(b)), pbData: &b[0]}
}

func (b dataBlob) toBytes() []byte {
	if b.pbData == nil || b.cbData == 0 {
		return nil
	}
	out := make([]byte, b.cbData)
	copy(out, unsafe.Slice(b.pbData, b.cbData))
	return out
}

func protect(data []byte) ([]byte, error) {
	in := newBlob(data)
	var out dataBlob
	r, _, err := procCryptProtect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // szDataDescr
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.toBytes(), nil
}

func unprotect(data []byte) ([]byte, error) {
	in := newBlob(data)
	var out dataBlob
	r, _, err := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // ppszDataDescr
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.toBytes(), nil
}
