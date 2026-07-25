// Package auth owns the JWT lifecycle for the LudoTrace desktop client.
//
// The opaque long-lived token is stored in the OS keychain by the keychain
// package. This package exchanges that token for short-lived JWTs via Core's
// POST /v1/auth/token endpoint. Clerk is never contacted directly — Core
// proxies all Clerk interaction server-side.
//
// Callers only ever see GetToken; they do not need to know Clerk exists.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/ludotrace/client/internal/browser"
	"github.com/ludotrace/client/internal/keychain"
	"github.com/ludotrace/client/internal/tracing"
)

// Sentinel errors that callers (e.g. tray) can check with errors.Is.
var (
	// ErrNotSignedIn means no opaque token is present; the user must sign in.
	ErrNotSignedIn = errors.New("auth: not signed in")

	// ErrTokenRevoked means Core rejected the stored opaque token.
	// The token has been removed from the keychain; the user must sign in again.
	ErrTokenRevoked = errors.New("auth: opaque token revoked or expired")

	// ErrSignedInDegraded is returned by SignIn when sign-in succeeded and the
	// session is fully usable, but the OS keychain was unavailable so the token
	// was saved to the encrypted on-disk fallback. The token still survives a
	// restart; callers may surface that the OS keychain needs attention.
	ErrSignedInDegraded = errors.New("auth: signed in, but OS keychain unavailable — token saved to encrypted fallback file")

	// ErrSignedInNotPersisted is returned by SignIn when sign-in succeeded and
	// the session is usable, but the token could not be persisted anywhere. The
	// user will have to sign in again after restarting the client.
	ErrSignedInNotPersisted = errors.New("auth: signed in, but credentials could not be saved — you will need to sign in again after restarting")
)

const (
	// proactiveRefreshBuffer is how long before expiry we treat a JWT as stale.
	proactiveRefreshBuffer = 10 * time.Second

	// signInTimeout is how long we wait for the user to complete the browser flow.
	signInTimeout = 5 * time.Minute

	// httpTimeout is the deadline for individual HTTP calls to Core.
	httpTimeout = 15 * time.Second
)

// tokenResponse is the expected JSON body from POST /v1/auth/token.
type tokenResponse struct {
	JWT       string `json:"jwt"`
	ExpiresIn int    `json:"expires_in"` // seconds
}

// Client is the interface other packages use. They call GetToken and never
// import anything Clerk-related.
type Client interface {
	// GetToken returns a currently-valid short-lived JWT, refreshing from Core
	// if the cached one is absent or near expiry. Concurrent callers near expiry
	// produce exactly one Core request; all block until it completes.
	GetToken(ctx context.Context) (string, error)

	// SignIn opens the system browser to Core's hosted sign-in page, starts a
	// local HTTP listener for the redirect callback, and on success stores the
	// opaque token returned by Core in the OS keychain.
	//
	// A nil return means fully signed in and persisted. SignIn may also return
	// ErrSignedInDegraded (signed in; token saved to the encrypted fallback
	// because the OS keychain was unavailable) or ErrSignedInNotPersisted
	// (signed in for this session only; token could not be saved). Both indicate
	// a usable session — callers should treat them as warnings, not failures.
	// Any other error means sign-in itself did not complete.
	SignIn(ctx context.Context) error

	// SignOut clears the in-memory JWT, removes the opaque token from the
	// keychain, and best-effort asks Core to revoke it server-side.
	SignOut(ctx context.Context) error

	// IsSignedIn reports whether a usable opaque token is present locally.
	// It does not make a network call.
	IsSignedIn() bool
}

// Config holds the external dependencies for the auth client.
type Config struct {
	CoreURL string
}

type authClient struct {
	coreURL string
	kc      keychain.Store
	httpCli *http.Client

	// mu guards jwt, jwtExpiry, and opaqueToken.
	mu          sync.Mutex
	jwt         string
	jwtExpiry   time.Time
	opaqueToken string // in-memory fallback when keychain save fails

	// refreshMu serializes Core calls. When one goroutine holds this lock,
	// others wait rather than firing duplicate requests. After the lock is
	// released, waiters re-check the cache (double-checked locking) and
	// return the already-refreshed JWT without hitting Core again.
	refreshMu sync.Mutex
}

// New returns a ready-to-use auth Client.
func New(cfg Config, kc keychain.Store) Client {
	return &authClient{
		coreURL: cfg.CoreURL,
		kc:      kc,
		httpCli: &http.Client{Timeout: httpTimeout},
	}
}

// GetToken returns a valid JWT. It refreshes from Core if the cached token is
// absent or within proactiveRefreshBuffer of expiry.
//
// Single-flight guarantee: concurrent callers that all find the cache stale
// will serialize on refreshMu. The first one through fetches a new JWT; the
// rest re-check the cache afterward and return the cached result, so Core
// receives exactly one request per refresh cycle.
func (c *authClient) GetToken(ctx context.Context) (string, error) {
	// Fast path: cache hit.
	if jwt, ok := c.cachedJWT(); ok {
		return jwt, nil
	}

	// Slow path: need a refresh. Serialize via refreshMu.
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	// Re-check after acquiring the lock — a previous waiter may have already
	// refreshed while we were blocked.
	if jwt, ok := c.cachedJWT(); ok {
		return jwt, nil
	}

	return c.fetchJWT(ctx)
}

// SignIn starts the browser-based sign-in flow.
//
//  1. Binds to 127.0.0.1:{ephemeral port} for the OAuth redirect callback.
//  2. Opens the system browser to {coreURL}/auth/signin?redirect_uri=...&state=...
//  3. Core handles the Clerk flow and redirects back with ?token=<opaque>&state=<nonce>.
//  4. Stores the opaque token in the keychain.
//
// state is a fresh random nonce per call, verified against what the callback
// receives before any token is accepted (RFC 6749 §10.12). Without it, the
// ephemeral loopback port is the only thing separating "signed in as
// yourself" from "signed in as whoever's crafted redirect reached this
// listener first" — and the port space is small enough to be scanned within
// signInTimeout.
func (c *authClient) SignIn(ctx context.Context) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("auth: open callback listener: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	state, err := generateState()
	if err != nil {
		return fmt.Errorf("auth: generate state: %w", err)
	}

	tokenCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintln(w, "Sign-in failed: state mismatch.")
			select {
			case errCh <- fmt.Errorf("auth: callback state mismatch"):
			default:
			}
			return
		}
		token := r.URL.Query().Get("token")
		if token == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintln(w, "Sign-in failed: missing token parameter.")
			select {
			case errCh <- fmt.Errorf("auth: callback received no token"):
			default:
			}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintln(w, `<!DOCTYPE html><html><body><p>Signed in to LudoTrace. You may close this tab.</p></body></html>`)
		select {
		case tokenCh <- token:
		default:
		}
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	signInURL := c.coreURL + "/auth/signin?redirect_uri=" + url.QueryEscape(redirectURI) + "&state=" + url.QueryEscape(state)
	// Log the exact URL we hand to the OS. openBrowser succeeds as soon as the
	// launcher process starts, so a mangled URL (e.g. the old Windows cmd.exe
	// "&" truncation that dropped the state param) leaves no other trace — this
	// line is the only signal of what was actually requested.
	slog.Info("auth: opening sign-in URL", "url", signInURL)
	if err := browser.Open(signInURL); err != nil {
		return fmt.Errorf("auth: open browser: %w", err)
	}

	timer := time.NewTimer(signInTimeout)
	defer timer.Stop()

	select {
	case token := <-tokenCh:
		// Store in memory regardless of keychain outcome so the session works
		// immediately.
		c.mu.Lock()
		c.opaqueToken = token
		c.mu.Unlock()

		// Persist. keychain.New chains the OS keychain → encrypted file fallback.
		switch err := c.kc.Save(token); {
		case err == nil:
			slog.Info("auth: opaque token saved to OS keychain", "token_bytes", len(token))
			return nil
		case errors.Is(err, keychain.ErrUsedFallback):
			// Durable, but the OS keychain is broken (e.g. Windows Credential
			// Manager full → ERROR_NOT_ENOUGH_MEMORY). Warn loudly; survives restart.
			slog.Warn("auth: OS keychain unavailable; token saved to encrypted fallback file",
				"err", err, "token_bytes", len(token))
			return ErrSignedInDegraded
		default:
			// Nothing persisted: in-memory only, lost on restart.
			slog.Error("auth: could not persist token; in-memory only, sign-in will NOT survive restart",
				"err", err, "token_bytes", len(token))
			return ErrSignedInNotPersisted
		}
	case err := <-errCh:
		return err
	case <-timer.C:
		return fmt.Errorf("auth: sign-in timed out after %s", signInTimeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// generateState returns a cryptographically random hex nonce for the OAuth
// state parameter.
func generateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand read: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// SignOut clears local state and best-effort revokes the opaque token on Core.
// Always removes the keychain entry; errors from Core revocation are logged
// but not returned (the local sign-out is unconditional).
func (c *authClient) SignOut(ctx context.Context) error {
	// Best-effort server-side revocation.
	if opaque, err := c.kc.Load(); err == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.coreURL+"/v1/auth/token", nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+opaque)
			if resp, err := c.httpCli.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}

	c.mu.Lock()
	c.jwt = ""
	c.jwtExpiry = time.Time{}
	c.opaqueToken = ""
	c.mu.Unlock()

	return c.kc.Delete()
}

// IsSignedIn reports whether a usable opaque token is available (memory or keychain).
// Does not validate against Core.
func (c *authClient) IsSignedIn() bool {
	if _, ok := c.cachedJWT(); ok {
		return true
	}
	_, err := c.loadOpaque()
	return err == nil
}

// cachedJWT returns the in-memory JWT if it is present and not near expiry.
func (c *authClient) cachedJWT() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jwt != "" && time.Until(c.jwtExpiry) > proactiveRefreshBuffer {
		return c.jwt, true
	}
	return "", false
}

// loadOpaque returns the opaque token from the in-memory fallback or keychain.
func (c *authClient) loadOpaque() (string, error) {
	c.mu.Lock()
	mem := c.opaqueToken
	c.mu.Unlock()
	if mem != "" {
		return mem, nil
	}
	tok, err := c.kc.Load()
	if errors.Is(err, keychain.ErrNotFound) {
		return "", ErrNotSignedIn
	}
	if err != nil {
		return "", fmt.Errorf("auth: load opaque token: %w", err)
	}
	return tok, nil
}

// fetchJWT loads the opaque token, calls Core's token endpoint, caches the
// result, and returns the JWT. Must be called with refreshMu held.
func (c *authClient) fetchJWT(ctx context.Context) (string, error) {
	opaque, err := c.loadOpaque()
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.coreURL+"/v1/auth/token", nil)
	if err != nil {
		return "", fmt.Errorf("auth: build token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+opaque)
	// client#64, companion to core#80: root Core's trace at this request
	// rather than letting it mint its own. Best-effort — a rand failure just
	// means this call goes untraced, never blocks the token exchange.
	if traceID, err := tracing.NewTraceID(); err == nil {
		if traceParent, err := tracing.TraceParent(traceID); err == nil {
			req.Header.Set("traceparent", traceParent)
		}
	}

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return "", fmt.Errorf("auth: token request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var tr tokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
			return "", fmt.Errorf("auth: decode token response: %w", err)
		}
		if tr.JWT == "" || tr.ExpiresIn <= 0 {
			return "", fmt.Errorf("auth: invalid token response from Core (empty jwt or non-positive expires_in)")
		}
		expiry := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
		c.mu.Lock()
		c.jwt = tr.JWT
		c.jwtExpiry = expiry
		c.mu.Unlock()
		return tr.JWT, nil

	case http.StatusUnauthorized:
		// Opaque token is invalid or revoked. Clear everything so the tray
		// shows "Sign In" rather than retrying forever.
		c.mu.Lock()
		c.jwt = ""
		c.jwtExpiry = time.Time{}
		c.opaqueToken = ""
		c.mu.Unlock()
		_ = c.kc.Delete()
		return "", ErrTokenRevoked

	default:
		return "", fmt.Errorf("auth: unexpected %d from Core /v1/auth/token", resp.StatusCode)
	}
}
