package knowngames

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ludotrace/client/internal/config"
)

// fakeAuthClient is a minimal auth.Client stub: List only ever calls
// GetToken, so the other methods just need to satisfy the interface.
type fakeAuthClient struct {
	token string
	err   error
}

func (f *fakeAuthClient) GetToken(ctx context.Context) (string, error) { return f.token, f.err }
func (f *fakeAuthClient) SignIn(ctx context.Context) error             { return nil }
func (f *fakeAuthClient) SignOut(ctx context.Context) error            { return nil }
func (f *fakeAuthClient) IsSignedIn() bool                             { return true }

func withTempConfigDir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestList_LiveFetchWritesCache(t *testing.T) {
	withTempConfigDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer tok")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"games": []Game{{GameID: "fallout4", Name: "Fallout 4"}},
		})
	}))
	defer srv.Close()

	games, err := List(context.Background(), srv.URL, &fakeAuthClient{token: "tok"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(games) != 1 || games[0].GameID != "fallout4" {
		t.Fatalf("List = %+v, want one fallout4 entry", games)
	}

	cached, err := ReadCache()
	if err != nil {
		t.Fatalf("ReadCache after successful List: %v", err)
	}
	if len(cached) != 1 || cached[0].GameID != "fallout4" {
		t.Fatalf("ReadCache = %+v, want the fetched list", cached)
	}
}

func TestList_FallsBackToCacheOnFetchError(t *testing.T) {
	withTempConfigDir(t)

	// Warm the cache with a prior successful call.
	path, err := config.KnownGamesCachePath()
	if err != nil {
		t.Fatalf("KnownGamesCachePath: %v", err)
	}
	if err := writeCache([]Game{{GameID: "stardew", Name: "Stardew Valley"}}); err != nil {
		t.Fatalf("writeCache: %v", err)
	}
	_ = path

	games, err := List(context.Background(), "http://127.0.0.1:0", &fakeAuthClient{err: errors.New("not signed in")})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(games) != 1 || games[0].GameID != "stardew" {
		t.Fatalf("List = %+v, want the cached stardew entry", games)
	}
}

func TestList_ErrorsWhenNoFetchAndNoCache(t *testing.T) {
	withTempConfigDir(t)

	_, err := List(context.Background(), "http://127.0.0.1:0", &fakeAuthClient{err: errors.New("not signed in")})
	if err == nil {
		t.Fatal("List: want error when neither fetch nor cache is available, got nil")
	}
}
