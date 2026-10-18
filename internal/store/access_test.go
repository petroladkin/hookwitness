package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func TestAccessMigrationKeepsData(t *testing.T) {
	ctx := context.Background()
	p := tempDB(t)
	// A database of the previous version: migrations up to 4.
	old := fstest.MapFS{}
	ms, err := loadMigrations(migrationFS)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.version < 5 {
			old["migrations/"+m.name] = &fstest.MapFile{Data: []byte(m.sql)}
		}
	}
	s, err := open(ctx, p, old)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := s.CreateEndpoint(ctx, "/kept")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.SchemaVersion(ctx); v < 5 {
		t.Fatalf("schema %d after reopening", v)
	}
	got, err := s.Endpoint(ctx, ep.ID)
	if err != nil || got.Path != "/kept" {
		t.Fatalf("endpoint after the migration: %+v, %v", got, err)
	}
	if on, err := s.HasUsers(ctx); err != nil || on {
		t.Fatalf("HasUsers on a migrated database = %v, %v", on, err)
	}
}

func TestUsers(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if on, _ := s.HasUsers(ctx); on {
		t.Fatal("a new database has users")
	}
	a, err := s.AddUser(ctx, "alice", RoleAdmin, "rec-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUser(ctx, "alice", RoleUser, "x"); !errors.Is(err, ErrExists) {
		t.Fatalf("a taken name: %v", err)
	}
	if on, _ := s.HasUsers(ctx); !on {
		t.Fatal("HasUsers false after AddUser")
	}
	got, err := s.UserByName(ctx, "alice")
	if err != nil || got.ID != a.ID || got.Role != RoleAdmin || got.Password != "rec-a" {
		t.Fatalf("UserByName: %+v, %v", got, err)
	}
	if _, err := s.UserByName(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no such user: %v", err)
	}
	if _, err := s.SetRole(ctx, "alice", RoleUser); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("demoting the last admin: %v", err)
	}
	if _, err := s.RemoveUser(ctx, "alice"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("removing the last admin: %v", err)
	}
	if _, err := s.AddUser(ctx, "bob", RoleUser, "rec-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddToken(ctx, "bbbbbbbb", mustUser(t, s, "bob").ID, "ci", "h"); err != nil {
		t.Fatal(err)
	}
	us, err := s.Users(ctx)
	if err != nil || len(us) != 2 || us[0].Name != "alice" || us[1].Tokens != 1 {
		t.Fatalf("Users: %+v, %v", us, err)
	}
	if u, err := s.SetRole(ctx, "bob", RoleAdmin); err != nil || u.Role != RoleAdmin {
		t.Fatalf("promote: %+v, %v", u, err)
	}
	if _, err := s.SetRole(ctx, "alice", RoleUser); err != nil {
		t.Fatalf("demote with another admin: %v", err)
	}
	if _, err := s.RemoveUser(ctx, "bob"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("bob is the last admin now: %v", err)
	}
	if _, err := s.RemoveUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveUser(ctx, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing twice: %v", err)
	}
}

func mustUser(t *testing.T, s *Store, name string) User {
	t.Helper()
	u, err := s.UserByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// Two admins demoted at once: exactly one demotion succeeds.
func TestLastAdminUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	for _, n := range []string{"a1", "a2"} {
		if _, err := s.AddUser(ctx, n, RoleAdmin, "r"); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, n := range []string{"a1", "a2"} {
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = s.SetRole(ctx, n, RoleUser) }()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrLastAdmin):
			t.Fatal(err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d demotions succeeded, want 1 (%v)", ok, errs)
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	u, _ := s.AddUser(ctx, "carol", RoleAdmin, "r")
	other, _ := s.AddUser(ctx, "dave", RoleUser, "r")
	s1, err := s.AddSession(ctx, []byte("h1"), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := s.AddSession(ctx, []byte("h2"), u.ID)
	if _, err := s.AddSession(ctx, []byte("h3"), other.ID); err != nil {
		t.Fatal(err)
	}
	se, err := s.SessionByHash(ctx, []byte("h1"))
	if err != nil || se.ID != s1 || se.Name != "carol" || se.Role != RoleAdmin {
		t.Fatalf("SessionByHash: %+v, %v", se, err)
	}
	later := time.Now().Add(time.Hour)
	if err := s.TouchSession(ctx, s1, later); err != nil {
		t.Fatal(err)
	}
	if se, _ := s.SessionByHash(ctx, []byte("h1")); se.LastSeenAt.UnixMilli() != later.UnixMilli() {
		t.Fatalf("touch: %v", se.LastSeenAt)
	}
	// A password change keeps the session that made it and ends the others.
	if _, err := s.SetPassword(ctx, "carol", "r2", s1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByHash(ctx, []byte("h2")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other session after a password change: %v (id %d)", err, s2)
	}
	if _, err := s.SessionByHash(ctx, []byte("h1")); err != nil {
		t.Fatalf("own session after a password change: %v", err)
	}
	// A role change ends every session of that user.
	if _, err := s.SetRole(ctx, "dave", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByHash(ctx, []byte("h3")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session after a role change: %v", err)
	}
	if err := s.EndSession(ctx, s1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByHash(ctx, []byte("h1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ended session: %v", err)
	}
	// Sweep: idle and too old ones go.
	a, _ := s.AddSession(ctx, []byte("old"), u.ID)
	s.TouchSession(ctx, a, time.Now().Add(-48*time.Hour))
	s.AddSession(ctx, []byte("new"), u.ID)
	if err := s.SweepSessions(ctx, time.Now().Add(-24*time.Hour), time.Now().Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByHash(ctx, []byte("old")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("idle session survived the sweep: %v", err)
	}
	if _, err := s.SessionByHash(ctx, []byte("new")); err != nil {
		t.Fatalf("fresh session swept: %v", err)
	}
	// Removing a user ends their sessions.
	if _, err := s.SetRole(ctx, "carol", RoleUser); err != nil { // dave is an admin now
		t.Fatal(err)
	}
	if _, err := s.RemoveUser(ctx, "carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByHash(ctx, []byte("new")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session of a removed user: %v", err)
	}
}

func TestTokens(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	u, _ := s.AddUser(ctx, "erin", RoleAdmin, "r")
	v, _ := s.AddUser(ctx, "finn", RoleUser, "r")
	tk, err := s.AddToken(ctx, "aaaaaaaa", u.ID, "ci", "hash-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddToken(ctx, "cccccccc", u.ID, "ci", "x"); !errors.Is(err, ErrExists) {
		t.Fatalf("a taken token name: %v", err)
	}
	if _, err := s.AddToken(ctx, "dddddddd", v.ID, "ci", "hash-d"); err != nil {
		t.Fatalf("the same name for another owner: %v", err)
	}
	got, err := s.TokenByPublicID(ctx, "aaaaaaaa")
	if err != nil || got.ID != tk.ID || got.Hash != "hash-a" || got.Owner != "erin" || got.Role != RoleAdmin {
		t.Fatalf("TokenByPublicID: %+v, %v", got, err)
	}
	if !got.LastUsedAt.IsZero() {
		t.Fatal("a new token was used")
	}
	s.TouchToken(ctx, tk.ID, time.Now())
	if got, _ := s.TokenByPublicID(ctx, "aaaaaaaa"); got.LastUsedAt.IsZero() {
		t.Fatal("TouchToken did not record the use")
	}
	mine, err := s.Tokens(ctx, u.ID)
	if err != nil || len(mine) != 1 || mine[0].Hash != "" {
		t.Fatalf("Tokens(owner): %+v, %v", mine, err)
	}
	all, _ := s.Tokens(ctx, 0)
	if len(all) != 2 {
		t.Fatalf("Tokens(all): %d", len(all))
	}
	if _, err := s.RevokeToken(ctx, "dddddddd", u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking another owner's token as an owner: %v", err)
	}
	if _, err := s.RevokeToken(ctx, "dddddddd", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TokenByPublicID(ctx, "dddddddd"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked token found: %v", err)
	}
	// The owner's role is read as stored now.
	if _, err := s.SetRole(ctx, "finn", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRole(ctx, "erin", RoleUser); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.TokenByPublicID(ctx, "aaaaaaaa"); got.Role != RoleUser {
		t.Fatalf("token role after demotion: %s", got.Role)
	}
	if _, err := s.RemoveUser(ctx, "erin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TokenByPublicID(ctx, "aaaaaaaa"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("token of a removed owner: %v", err)
	}
}
