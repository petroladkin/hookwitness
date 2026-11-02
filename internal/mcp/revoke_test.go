package mcp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/api"
	"github.com/petroladkin/hookwitness/internal/live"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

// tokenServer is MCP at /admin/mcp with login on: a request carries a
// personal token, resolved as internal/web's gate does (the identity in the
// request's context and in the SDK's TokenInfo).
type tokenServer struct {
	srv  *httptest.Server
	h    *Streamable
	st   *store.Store
	auth *access.Authenticator
	user store.User
}

func newTokenServer(t *testing.T) *tokenServer {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "hw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	keys, _ := secret.Load("", filepath.Join(dir, "hw.db.key"))
	auth := access.New(st, keys, "")
	u, err := st.AddUser(context.Background(), "ann", store.RoleAdmin, auth.HashPassword("correct horse battery"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := api.New(st, live.New(), log, api.Options{Prefix: "/admin", Reserved: "/admin", MaxWait: time.Minute, Keys: keys, Keyring: keys,
		Stop: make(chan struct{}), Version: "test", Access: auth})
	h := Handler(a, api.Base("/admin"), "test", log, nil, 0, auth)
	verify := func(_ context.Context, _ string, r *http.Request) (*mcpauth.TokenInfo, error) {
		id := access.From(r.Context())
		if id == nil {
			return nil, mcpauth.ErrInvalidToken
		}
		return &mcpauth.TokenInfo{UserID: "user:1", Extra: map[string]any{access.MCPExtra: id}}, nil
	}
	authed := mcpauth.RequireBearerToken(verify, &mcpauth.RequireBearerTokenOptions{AllowMissingExpiration: true})(h)
	mux := http.NewServeMux()
	mux.Handle("/admin/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, _, err := auth.Bearer(r.Context(), r.Header.Get("Authorization")); err == nil && id != nil {
			r = r.WithContext(access.With(r.Context(), id))
		}
		authed.ServeHTTP(w, r)
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { h.CloseSessions(); srv.Close() })
	return &tokenServer{srv, h, st, auth, u}
}

type bearer struct{ tok string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

// connect opens a session with a new token of ann; it returns the session
// and the token's public id.
func (ts *tokenServer) connect(t *testing.T, name string) (*sdk.ClientSession, string) {
	t.Helper()
	v, tok, err := ts.auth.MintFor(context.Background(), ts.user.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	cl := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := cl.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: ts.srv.URL + "/admin/mcp",
		HTTPClient: &http.Client{Transport: bearer{v}}, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, tok.PublicID
}

// A token revoked by another process (the user command; nothing is
// announced): a running wait ends at its next round with an error. (The
// periodic check is an hour away here, so the error is the round's.)
func TestRevokedTokenEndsWaits(t *testing.T) {
	oldWait, oldCheck := apiWait, credentialCheck
	apiWait, credentialCheck = 100*time.Millisecond, time.Hour
	t.Cleanup(func() { apiWait, credentialCheck = oldWait, oldCheck })
	ts := newTokenServer(t)
	cs, public := ts.connect(t, "agent")
	if _, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "create_endpoint", Arguments: map[string]any{"path": "/hooks"}}); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		res *sdk.CallToolResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "expect", Arguments: map[string]any{"endpoint": "/hooks", "within_s": 120}})
		done <- answer{res, err}
	}()
	select {
	case a := <-done:
		t.Fatalf("expect ended before the revocation: %v %v", a.err, a.res)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := ts.st.RevokeToken(context.Background(), public, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-done:
		if a.err != nil || !a.res.IsError || !strings.Contains(toolText(a.res), "the credential was revoked") {
			t.Fatalf("expect after the revocation: %v %s", a.err, toolText(a.res))
		}
	case <-time.After(10 * time.Second): // the alternative is the 120 s it was asked to wait
		t.Fatal("expect still waits after its token was revoked")
	}
}

// The same, and the periodic check closes the revoked token's session;
// another token's stays.
func TestPeriodicCheckClosesSessions(t *testing.T) {
	old := credentialCheck
	credentialCheck = 50 * time.Millisecond
	t.Cleanup(func() { credentialCheck = old })
	ts := newTokenServer(t)
	_, revoked := ts.connect(t, "revoked")
	kept, _ := ts.connect(t, "kept")
	waitFor(t, "two sessions", func() bool { return ts.h.Sessions() == 2 })
	time.Sleep(200 * time.Millisecond) // checks pass with both standing
	if ts.h.Sessions() != 2 {
		t.Fatalf("a check closed a standing session: %d", ts.h.Sessions())
	}
	if _, err := ts.st.RevokeToken(context.Background(), revoked, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the revoked token's session closed", func() bool { return ts.h.Sessions() == 1 })
	if _, err := kept.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("the other token's session: %v", err)
	}
}

// A token revoked in this process (Changed): its session closes at once,
// before any periodic check; another token's session stays.
func TestRevocationClosesSessionsAtOnce(t *testing.T) {
	old := credentialCheck
	credentialCheck = time.Hour
	t.Cleanup(func() { credentialCheck = old })
	ts := newTokenServer(t)
	_, revoked := ts.connect(t, "revoked")
	kept, _ := ts.connect(t, "kept")
	waitFor(t, "two sessions", func() bool { return ts.h.Sessions() == 2 })
	if _, err := ts.st.RevokeToken(context.Background(), revoked, 0); err != nil {
		t.Fatal(err)
	}
	ts.auth.Changed()
	waitFor(t, "the revoked token's session closed", func() bool { return ts.h.Sessions() == 1 })
	if _, err := kept.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("the other token's session: %v", err)
	}
}
