package tray

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ludotrace/client/internal/auth"
)

// stubAuth is an auth.Client whose SignIn returns a fixed error. Only SignIn
// is exercised by doSignIn; the rest satisfy the interface.
type stubAuth struct {
	signInErr error
}

func (s stubAuth) GetToken(context.Context) (string, error) { return "", nil }
func (s stubAuth) SignIn(context.Context) error             { return s.signInErr }
func (s stubAuth) SignOut(context.Context) error            { return nil }
func (s stubAuth) IsSignedIn() bool                         { return true }

// signaled reports whether a signed-in wake is pending on the channel.
func signaled(t *Tray) bool {
	select {
	case <-t.signedInCh:
		return true
	default:
		return false
	}
}

// Every SignIn outcome that yields a usable session must wake a worker parked
// on SignedInCh — the degraded and not-persisted cases included, since both
// leave the client able to upload. Missing one leaves the worker asleep with
// valid credentials until the fallback timer expires.
func TestDoSignIn_SignalsOnEveryUsableSession(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantSignal bool
	}{
		{"persisted", nil, true},
		{"degraded", auth.ErrSignedInDegraded, true},
		{"wrapped degraded", fmt.Errorf("sign-in: %w", auth.ErrSignedInDegraded), true},
		{"not persisted", auth.ErrSignedInNotPersisted, true},
		{"failed", errors.New("sign-in timed out"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := New(stubAuth{signInErr: c.err}, nil, "", "", "")
			tr.doSignIn()
			if got := signaled(tr); got != c.wantSignal {
				t.Errorf("signed-in signal = %v, want %v", got, c.wantSignal)
			}
		})
	}
}

// The signal is coalesced, not accumulated: a second sign-in before the worker
// consumes the first must not block the tray goroutine.
func TestSignalSignedIn_Coalesces(t *testing.T) {
	tr := New(nil, nil, "", "", "")
	tr.signalSignedIn()
	tr.signalSignedIn()

	if !signaled(tr) {
		t.Fatal("no signal pending after two sends")
	}
	if signaled(tr) {
		t.Error("second signal was buffered; sends should coalesce")
	}
}
