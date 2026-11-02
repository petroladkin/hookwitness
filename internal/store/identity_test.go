package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/fstest"
)

// A database of the previous schema gains the email column, its index and
// the identities table; its users, sessions and tokens are untouched.
func TestOIDCMigration(t *testing.T) {
	ctx := context.Background()
	p := tempDB(t)
	old := fstest.MapFS{}
	ms, _ := loadMigrations(migrationFS)
	for _, m := range ms {
		if m.version < 12 {
			old["migrations/"+m.name] = &fstest.MapFile{Data: []byte(m.sql)}
		}
	}
	s, err := open(ctx, p, old)
	if err != nil {
		t.Fatal(err)
	}
	// Rows as the previous version writes them (no email column yet).
	var uid int64
	if err := s.db.QueryRowContext(ctx, `INSERT INTO users (name, role, password, created_at, password_changed_at)
		VALUES ('ann', 'admin', 'hk1-pbkdf2-sha256$x', 1, 1) RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddSession(ctx, []byte("h"), uid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddToken(ctx, "pub", uid, "ci", "hk1-pbkdf2-sha256$y"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.UserByName(ctx, "ann")
	if err != nil || got.Email != "" || !got.HasPassword() || got.Password != "hk1-pbkdf2-sha256$x" {
		t.Fatalf("ann after the migration: %+v %v", got, err)
	}
	var sessions, tokens, tables int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&sessions)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens`).Scan(&tokens)
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name IN ('identities', 'users_by_email')`).Scan(&tables)
	if sessions != 1 || tokens != 1 || tables != 2 {
		t.Fatalf("sessions %d, tokens %d, new objects %d", sessions, tokens, tables)
	}
}

func TestEmails(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	ann, err := s.AddUserWithEmail(ctx, "ann", RoleAdmin, "", "Ann@Example.COM")
	if err != nil || ann.Email != "ann@example.com" || ann.HasPassword() {
		t.Fatalf("add with email: %+v %v", ann, err)
	}
	if _, err := s.AddUserWithEmail(ctx, "ann2", RoleUser, "", "ANN@example.com"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("a taken email on add: %v", err)
	}
	if _, err := s.AddUserWithEmail(ctx, "ann", RoleUser, "", "other@example.com"); !errors.Is(err, ErrExists) {
		t.Fatalf("a taken name: %v", err)
	}
	bob, _ := s.AddUser(ctx, "bob", RoleUser, "hk1-pbkdf2-sha256$x")
	if _, err := s.SetEmail(ctx, "bob", "ann@example.com"); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("a taken email on set: %v", err)
	}
	if u, err := s.SetEmail(ctx, "bob", "Bob@Example.com"); err != nil || u.Email != "bob@example.com" {
		t.Fatalf("set: %+v %v", u, err)
	}
	if u, err := s.UserByEmail(ctx, "BOB@example.com"); err != nil || u.ID != bob.ID {
		t.Fatalf("by email: %+v %v", u, err)
	}
	if _, err := s.SetEmail(ctx, "bob", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByEmail(ctx, "bob@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cleared: %v", err)
	}
	if _, err := s.SetEmail(ctx, "nobody", "x@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no such user: %v", err)
	}
	if us, _ := s.Users(ctx); len(us) != 2 || us[0].Email != "ann@example.com" || us[1].Email != "" {
		t.Fatalf("users %+v", us)
	}
}

func TestSignIn(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	const google, entra = "https://accounts.google.test", "https://login.entra.test/tenant/v2.0"
	in := func(sub, email string, verified, create bool) SignInInput {
		return SignInInput{Issuer: google, Subject: sub, Provider: "google", Email: email, Verified: verified, AllowCreate: create, Name: "carol"}
	}
	if _, err := s.SignIn(ctx, in("s-0", "first@example.com", true, true)); !errors.Is(err, ErrLoginOff) {
		t.Fatalf("no users yet: %v", err)
	}
	ann, _ := s.AddUserWithEmail(ctx, "ann", RoleAdmin, "hk1-pbkdf2-sha256$x", "ann@example.com")
	s.AddUser(ctx, "carol", RoleUser, "hk1-pbkdf2-sha256$x") // takes the derived name

	// A verified email links the pre-created user once.
	r, err := s.SignIn(ctx, in("s-ann", "ANN@example.com", true, false))
	if err != nil || r.User.ID != ann.ID || !r.Linked || r.Created {
		t.Fatalf("first link: %+v %v", r, err)
	}
	// From then on the identity decides, whatever the email says now.
	s.AddUserWithEmail(ctx, "dave", RoleUser, "hk1-pbkdf2-sha256$x", "dave@example.com")
	r, err = s.SignIn(ctx, in("s-ann", "dave@example.com", false, false))
	if err != nil || r.User.ID != ann.ID || r.Linked {
		t.Fatalf("a known identity with another email: %+v %v", r, err)
	}
	if ids, _ := s.Identities(ctx, ann.ID); len(ids) != 1 || ids[0].Email != "dave@example.com" || ids[0].Subject != "s-ann" {
		t.Fatalf("identity after: %+v", ids)
	}
	for _, c := range []struct {
		name string
		in   SignInInput
		want error
	}{
		{"unverified", in("s-1", "dave@example.com", false, true), ErrUnverified},
		{"no email", in("s-2", "", true, true), ErrUnverified},
		{"no account", in("s-3", "nobody@example.com", true, false), ErrNoAccount},
		{"a second identity at the same issuer", in("s-4", "ann@example.com", true, false), ErrLinkedElsewhere},
	} {
		if _, err := s.SignIn(ctx, c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// Another issuer may link the same user.
	if r, err := s.SignIn(ctx, SignInInput{Issuer: entra, Subject: "e-ann", Provider: "entra", Email: "ann@example.com", Verified: true}); err != nil || r.User.ID != ann.ID {
		t.Fatalf("a second provider: %+v %v", r, err)
	}
	// An allowed domain creates a user without a password, named from the
	// email, -2 when taken.
	r, err = s.SignIn(ctx, in("s-carol", "Carol@Example.com", true, true))
	if err != nil || !r.Created || r.User.Name != "carol-2" || r.User.Role != RoleUser || r.User.HasPassword() || r.User.Email != "carol@example.com" {
		t.Fatalf("created: %+v %v", r, err)
	}
}

// Two first sign-ins of one identity at once make one user.
func TestSignInConcurrent(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	s.AddUser(ctx, "admin", RoleAdmin, "hk1-pbkdf2-sha256$x")
	var wg sync.WaitGroup
	ids := make([]int64, 8)
	for i := range ids {
		wg.Go(func() {
			r, err := s.SignIn(ctx, SignInInput{Issuer: "https://idp.test", Subject: "same", Provider: "idp", Email: "new@example.com",
				Verified: true, AllowCreate: true, Name: "new"})
			if err != nil {
				t.Error(err)
			}
			ids[i] = r.User.ID
		})
	}
	wg.Wait()
	us, _ := s.Users(ctx)
	all, _ := s.AllIdentities(ctx)
	if len(us) != 2 || len(all) != 1 || len(all[ids[0]]) != 1 {
		t.Fatalf("%d users, identities %+v", len(us), all)
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("different users: %v", ids)
		}
	}
}

func TestUnlink(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	s.AddUser(ctx, "admin", RoleAdmin, "hk1-pbkdf2-sha256$x")
	r, _ := s.SignIn(ctx, SignInInput{Issuer: "https://a.test", Subject: "1", Provider: "a", Email: "eve@example.com", Verified: true, AllowCreate: true, Name: "eve"})
	s.SignIn(ctx, SignInInput{Issuer: "https://b.test", Subject: "2", Provider: "b", Email: "eve@example.com", Verified: true})
	s.AddSession(ctx, []byte("eve-session"), r.User.ID)
	// One of two identities: allowed, and the user's sessions end.
	gone, err := s.Unlink(ctx, "eve", "https://a.test", true)
	if err != nil || gone.Provider != "a" {
		t.Fatalf("unlink one of two: %+v %v", gone, err)
	}
	var sessions int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, r.User.ID).Scan(&sessions)
	if sessions != 0 {
		t.Fatalf("%d sessions left after an unlink", sessions)
	}
	// The last way in: refused for the user, allowed for an admin.
	if _, err := s.Unlink(ctx, "eve", "https://b.test", true); !errors.Is(err, ErrLastWayIn) {
		t.Fatalf("own last way in: %v", err)
	}
	if _, err := s.Unlink(ctx, "eve", "https://b.test", false); err != nil {
		t.Fatalf("by an admin: %v", err)
	}
	if _, err := s.Unlink(ctx, "eve", "https://b.test", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("again: %v", err)
	}
	// Removing a user removes their identities.
	s.SignIn(ctx, SignInInput{Issuer: "https://a.test", Subject: "1", Provider: "a", Email: "eve@example.com", Verified: true})
	if _, err := s.RemoveUser(ctx, "eve"); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.AllIdentities(ctx); len(all) != 0 {
		t.Fatalf("identities left: %+v", all)
	}
}
