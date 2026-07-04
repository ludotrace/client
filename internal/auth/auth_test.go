package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ludotrace/client/internal/keychain"
)

// fakeStore is an in-memory keychain.Store for tests.
type fakeStore struct {
	token string
	has   bool
}

func (f *fakeStore) Save(token string) error {
	f.token = token
	f.has = true
	return nil
}

func (f *fakeStore) Load() (string, error) {
	if !f.has {
		return "", keychain.ErrNotFound
	}
	return f.token, nil
}

func (f *fakeStore) Delete() error {
	f.has = false
	f.token = ""
	return nil
}

func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func newClientWithToken(t *testing.T, handler http.HandlerFunc, token string) (*authClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	kc := &fakeStore{token: token, has: token != ""}
	c := New(Config{CoreURL: srv.URL}, kc).(*authClient)
	return c, srv
}

func TestGetToken_RefreshesWhenExpired(t *testing.T) {
	var calls int32
	c, srv := newClientWithToken(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("Authorization") != "Bearer opaque-tok" {
			t.Errorf("got Authorization %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, jsonBody(map[string]any{"jwt": "jwt-1", "expires_in": 60}))
	}, "opaque-tok")
	defer srv.Close()

	jwt, err := c.GetToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jwt != "jwt-1" {
		t.Fatalf("got jwt %q, want jwt-1", jwt)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("got %d calls to Core, want 1", got)
	}
}

func TestGetToken_NotCalledWhenCacheStillValid(t *testing.T) {
	var calls int32
	c, srv := newClientWithToken(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, jsonBody(map[string]any{"jwt": "jwt-1", "expires_in": 60}))
	}, "opaque-tok")
	defer srv.Close()

	if _, err := c.GetToken(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Second call should hit the in-memory cache, not Core, since the JWT is
	// still well within its 60s lifetime.
	jwt, err := c.GetToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jwt != "jwt-1" {
		t.Fatalf("got jwt %q, want jwt-1", jwt)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("got %d calls to Core, want 1 (cache should have been used)", got)
	}
}

func TestGetToken_RefreshesWhenNearExpiry(t *testing.T) {
	var calls int32
	c, srv := newClientWithToken(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, jsonBody(map[string]any{"jwt": fmt.Sprintf("jwt-%d", n), "expires_in": 60}))
	}, "opaque-tok")
	defer srv.Close()

	jwt1, err := c.GetToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Force the cached JWT to look stale (within the proactive refresh buffer).
	c.mu.Lock()
	c.jwtExpiry = time.Now().Add(proactiveRefreshBuffer / 2)
	c.mu.Unlock()

	jwt2, err := c.GetToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jwt1 == jwt2 {
		t.Fatalf("expected a refreshed jwt, got the same value %q twice", jwt1)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("got %d calls to Core, want 2", got)
	}
}

func TestGetToken_AuthFailure_RevokedNotRetriedForever(t *testing.T) {
	var calls int32
	c, srv := newClientWithToken(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}, "opaque-tok")
	defer srv.Close()

	_, err := c.GetToken(context.Background())
	if !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("got err %v, want ErrTokenRevoked", err)
	}
	if c.kc.(*fakeStore).has {
		t.Fatalf("expected keychain entry to be deleted after revocation")
	}

	// A second call with nothing left in the keychain must fail fast with
	// ErrNotSignedIn instead of hammering Core again.
	_, err = c.GetToken(context.Background())
	if !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("got err %v, want ErrNotSignedIn", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("got %d calls to Core, want 1 (second call should not retry against Core)", got)
	}
}

func TestGetToken_NoOpaqueToken_ErrNotSignedIn(t *testing.T) {
	c, srv := newClientWithToken(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("Core should not be contacted when there is no opaque token")
	}, "")
	defer srv.Close()

	_, err := c.GetToken(context.Background())
	if !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("got err %v, want ErrNotSignedIn", err)
	}
}

func TestGenerateState_UniqueAndNonEmpty(t *testing.T) {
	a, err := generateState()
	if err != nil {
		t.Fatalf("generateState: %v", err)
	}
	b, err := generateState()
	if err != nil {
		t.Fatalf("generateState: %v", err)
	}
	if a == "" || b == "" {
		t.Fatal("expected non-empty state values")
	}
	if a == b {
		t.Fatal("expected distinct state values across calls")
	}
}

func TestIsSignedIn(t *testing.T) {
	c, srv := newClientWithToken(t, func(w http.ResponseWriter, r *http.Request) {}, "opaque-tok")
	defer srv.Close()
	if !c.IsSignedIn() {
		t.Fatal("expected IsSignedIn() true with an opaque token present")
	}

	c2, srv2 := newClientWithToken(t, func(w http.ResponseWriter, r *http.Request) {}, "")
	defer srv2.Close()
	if c2.IsSignedIn() {
		t.Fatal("expected IsSignedIn() false with no opaque token present")
	}
}
