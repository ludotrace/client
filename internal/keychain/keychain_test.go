package keychain

import (
	"errors"
	"testing"
)

// memStore is a bare in-memory Store used to simulate the OS keychain and
// the on-disk fallback in isolation from any real backend.
type memStore struct {
	token   string
	present bool
	saveErr error
}

func (m *memStore) Save(token string) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.token = token
	m.present = true
	return nil
}

func (m *memStore) Load() (string, error) {
	if !m.present {
		return "", ErrNotFound
	}
	return m.token, nil
}

func (m *memStore) Delete() error {
	m.present = false
	m.token = ""
	return nil
}

func TestChainStore_SaveUsesPrimaryWhenAvailable(t *testing.T) {
	primary := &memStore{}
	fallback := &memStore{}
	c := &chainStore{primary: primary, fallback: fallback}

	if err := c.Save("tok"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !primary.present {
		t.Fatal("expected primary to hold the token")
	}
	if fallback.present {
		t.Fatal("expected fallback to be untouched when primary succeeds")
	}
}

func TestChainStore_SaveFallsBackWhenPrimaryFull(t *testing.T) {
	primary := &memStore{saveErr: errors.New("ERROR_NOT_ENOUGH_MEMORY")}
	fallback := &memStore{}
	c := &chainStore{primary: primary, fallback: fallback}

	err := c.Save("tok")
	if !errors.Is(err, ErrUsedFallback) {
		t.Fatalf("got err %v, want ErrUsedFallback", err)
	}
	if !fallback.present {
		t.Fatal("expected fallback to hold the token")
	}
	if got, _ := fallback.Load(); got != "tok" {
		t.Fatalf("got fallback token %q, want tok", got)
	}
}

func TestChainStore_SaveFailsWhenBothBackendsFail(t *testing.T) {
	primary := &memStore{saveErr: errors.New("primary unavailable")}
	fallback := &memStore{saveErr: errors.New("fallback unavailable")}
	c := &chainStore{primary: primary, fallback: fallback}

	err := c.Save("tok")
	if err == nil {
		t.Fatal("expected an error when both backends fail")
	}
	if errors.Is(err, ErrUsedFallback) {
		t.Fatal("did not expect ErrUsedFallback when the fallback itself failed")
	}
}

func TestChainStore_LoadPrefersPrimary(t *testing.T) {
	primary := &memStore{token: "primary-tok", present: true}
	fallback := &memStore{token: "fallback-tok", present: true}
	c := &chainStore{primary: primary, fallback: fallback}

	got, err := c.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "primary-tok" {
		t.Fatalf("got %q, want primary-tok", got)
	}
}

func TestChainStore_LoadFallsBackWhenPrimaryEmpty(t *testing.T) {
	primary := &memStore{}
	fallback := &memStore{token: "fallback-tok", present: true}
	c := &chainStore{primary: primary, fallback: fallback}

	got, err := c.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "fallback-tok" {
		t.Fatalf("got %q, want fallback-tok", got)
	}
}

func TestChainStore_LoadNotFoundWhenBothEmpty(t *testing.T) {
	c := &chainStore{primary: &memStore{}, fallback: &memStore{}}

	_, err := c.Load()
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got err %v, want ErrNotFound", err)
	}
}

func TestChainStore_SaveClearsStaleFallbackAfterPrimaryRecovers(t *testing.T) {
	// Regression: a token saved to the fallback while the OS keychain was
	// full must not shadow a later Load once the keychain is available
	// again and receives a fresh Save.
	primary := &memStore{}
	fallback := &memStore{token: "stale-tok", present: true}
	c := &chainStore{primary: primary, fallback: fallback}

	if err := c.Save("new-tok"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fallback.present {
		t.Fatal("expected stale fallback entry to be cleared once primary succeeds")
	}
	got, err := c.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "new-tok" {
		t.Fatalf("got %q, want new-tok", got)
	}
}

func TestChainStore_Delete(t *testing.T) {
	primary := &memStore{token: "tok", present: true}
	fallback := &memStore{token: "tok", present: true}
	c := &chainStore{primary: primary, fallback: fallback}

	if err := c.Delete(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if primary.present || fallback.present {
		t.Fatal("expected both backends cleared after Delete")
	}
}

func TestUnsupportedFallback_NeverPanicsWhenPrimaryUnavailable(t *testing.T) {
	// On platforms with no secure on-disk fallback (see fallback_other.go),
	// the chain must degrade to "in-memory only for this session" instead
	// of panicking — this is the bug a full/unavailable OS keychain shipped
	// a fix for.
	f := unsupportedFallback{}
	if err := f.Save("tok"); err == nil {
		t.Fatal("expected unsupportedFallback.Save to always fail")
	}
	if _, err := f.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got err %v, want ErrNotFound", err)
	}
	if err := f.Delete(); err != nil {
		t.Fatalf("unexpected error from Delete: %v", err)
	}

	c := &chainStore{primary: &memStore{saveErr: errors.New("keychain full")}, fallback: f}
	err := c.Save("tok")
	if err == nil {
		t.Fatal("expected an error when both primary and unsupported fallback fail")
	}
	if errors.Is(err, ErrUsedFallback) {
		t.Fatal("unsupportedFallback can never succeed, so ErrUsedFallback must not be reported")
	}
}
