package web

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/oidc/oidctest"
	"github.com/petroladkin/hookwitness/internal/store"
)

func (e *oidcEnv) page(t *testing.T, b *http.Client, path string) (int, string) {
	t.Helper()
	res, err := b.Get(e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func (e *oidcEnv) post(t *testing.T, b *http.Client, path string, f url.Values) (int, string, string) {
	t.Helper()
	res, err := b.PostForm(e.srv.URL+path, f)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header.Get("Location"), string(body)
}

// passwordLogin logs in with the test password in a new browser.
func (e *oidcEnv) passwordLogin(t *testing.T, name string) *http.Client {
	t.Helper()
	b := browser()
	if code, _, body := e.post(t, b, "/admin/login", url.Values{"name": {name}, "password": {"a-password-of-the-test"}}); code != http.StatusSeeOther {
		t.Fatalf("login %s: %d %s", name, code, body)
	}
	return b
}

// The account page of a provider user: the email, the identity, no
// password; Unlink only with another way in; a first password without a
// current one from a fresh sign-in, refused from an older one.
func TestAccountOfAProviderUser(t *testing.T) {
	e := newOIDCEnv(t, "/admin", "example.test")
	e.addUser(t, "admin", store.RoleAdmin, "")
	e.idp.Next(oidctest.Claims{Subject: "s-pat", Email: "pat@example.test", EmailVerified: oidctest.Ptr(true)})
	old := browser()
	if got := e.signIn(t, old, "/admin/"); got.status != http.StatusSeeOther {
		t.Fatalf("%+v", got)
	}
	// That sign-in is past the window from now on.
	window := firstPasswordWindow
	firstPasswordWindow = time.Nanosecond
	time.Sleep(time.Millisecond)
	_, page := e.page(t, old, "/admin/account")
	for _, want := range []string{"pat@example.test", "<b>not set</b>: you sign in through a provider", "TestIdP", "your only way to log in", "Set a password"} {
		if !strings.Contains(page, want) {
			t.Errorf("the account page lacks %q", want)
		}
	}
	if strings.Contains(page, `action="/admin/account/unlink"`) {
		t.Fatal("Unlink offered for the only way in")
	}
	if code, _, _ := e.post(t, old, "/admin/account/unlink", url.Values{"issuer": {e.idp.URL}}); code != http.StatusConflict {
		t.Fatalf("unlinking the only way in: %d", code)
	}
	pw := url.Values{"new": {"a-first-password-0001"}, "again": {"a-first-password-0001"}}
	if code, _, body := e.post(t, old, "/admin/account/password", pw); code != http.StatusUnprocessableEntity || !strings.Contains(body, "sign in again to set a password") {
		t.Fatalf("a first password from an old session: %d", code)
	}
	firstPasswordWindow = window
	fresh := browser()
	e.idp.Next(oidctest.Claims{Subject: "s-pat"})
	e.signIn(t, fresh, "/admin/")
	if code, loc, body := e.post(t, fresh, "/admin/account/password", pw); code != http.StatusSeeOther || loc != "/admin/account?saved=password" {
		t.Fatalf("a first password from a fresh session: %d %s %s", code, loc, body)
	}
	u, _ := e.st.UserByName(context.Background(), "pat")
	if !u.HasPassword() {
		t.Fatal("no password set")
	}
	// With a password, Unlink is offered; unlinking ends the user's logins.
	_, page = e.page(t, fresh, "/admin/account")
	if !strings.Contains(page, `action="/admin/account/unlink"`) || !strings.Contains(page, "Change password") {
		t.Fatal("Unlink not offered with a password")
	}
	if code, loc, _ := e.post(t, fresh, "/admin/account/unlink", url.Values{"issuer": {e.idp.URL}}); code != http.StatusSeeOther || loc != "/admin/login" {
		t.Fatalf("unlink: %d %s", code, loc)
	}
	if code, _ := e.page(t, fresh, "/admin/account"); code != http.StatusSeeOther {
		t.Fatalf("the session after the unlink: %d", code)
	}
	if ids, _ := e.st.Identities(context.Background(), u.ID); len(ids) != 0 {
		t.Fatalf("identities left: %+v", ids)
	}
}

// The users page with providers: emails, the ways in, a user added with an
// email and no password, an email set and refused when taken, an identity
// unlinked (their logins end), and the admin's own last way in refused.
func TestUsersPageWithProviders(t *testing.T) {
	e := newOIDCEnv(t, "/admin")
	e.addUser(t, "ann", store.RoleAdmin, "ann@example.test")
	admin := e.passwordLogin(t, "ann")
	code, loc, body := e.post(t, admin, "/admin/users", url.Values{"name": {"pat"}, "role": {"user"}, "email": {"Pat@Example.test"}})
	if code != http.StatusSeeOther || loc != "/admin/users?saved=added" {
		t.Fatalf("add a passwordless user: %d %s %s", code, loc, body)
	}
	if code, _, body := e.post(t, admin, "/admin/users", url.Values{"name": {"quinn"}, "role": {"user"}}); code != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "a password is needed, or an email to sign in through a provider") {
		t.Fatalf("no password and no email: %d", code)
	}
	if code, _, body := e.post(t, admin, "/admin/users", url.Values{"name": {"quinn"}, "role": {"user"}, "email": {"pat@example.test"}}); code != http.StatusConflict ||
		!strings.Contains(body, "another user has this email") {
		t.Fatalf("a taken email: %d", code)
	}
	// pat signs in through the provider and is linked.
	e.idp.Next(oidctest.Claims{Subject: "s-pat", Email: "pat@example.test", EmailVerified: oidctest.Ptr(true)})
	pat := browser()
	if got := e.signIn(t, pat, "/admin/"); got.status != http.StatusSeeOther {
		t.Fatalf("pat: %+v", got)
	}
	_, page := e.page(t, admin, "/admin/users")
	for _, want := range []string{"<th>Email</th>", "<th>Sign-in</th>", "pat@example.test", "TestIdP", `action="/admin/users/pat/unlink"`, "Unlink TestIdP", "Set email"} {
		if !strings.Contains(page, want) {
			t.Errorf("the users page lacks %q", want)
		}
	}
	if code, _, body := e.post(t, admin, "/admin/users/ann/email", url.Values{"email": {"pat@example.test"}}); code != http.StatusConflict || !strings.Contains(body, "another user has this email") {
		t.Fatalf("set a taken email: %d", code)
	}
	if code, loc, _ := e.post(t, admin, "/admin/users/ann/email", url.Values{"email": {"Ann.New@Example.test"}}); code != http.StatusSeeOther || loc != "/admin/users?saved=email" {
		t.Fatalf("set an email: %d %s", code, loc)
	}
	if u, _ := e.st.UserByName(context.Background(), "ann"); u.Email != "ann.new@example.test" {
		t.Fatalf("ann's email %q", u.Email)
	}
	if code, loc, _ := e.post(t, admin, "/admin/users/pat/unlink", url.Values{"issuer": {e.idp.URL}}); code != http.StatusSeeOther || loc != "/admin/users?saved=unlinked" {
		t.Fatalf("unlink pat: %d %s", code, loc)
	}
	if code, _ := e.page(t, pat, "/admin/"); code != http.StatusSeeOther {
		t.Fatalf("pat's session after the unlink: %d", code)
	}
	// The admin's own last way in: an admin with only an identity.
	e.addUser(t, "zoe", store.RoleAdmin, "zoe@example.test")
	e.st.SetPassword(context.Background(), "ann", "", 0)
	e.idp.Next(oidctest.Claims{Subject: "s-ann", Email: "ann.new@example.test", EmailVerified: oidctest.Ptr(true)})
	ann := browser()
	e.signIn(t, ann, "/admin/")
	if code, _, body := e.post(t, ann, "/admin/users/ann/unlink", url.Values{"issuer": {e.idp.URL}}); code != http.StatusConflict ||
		!strings.Contains(body, "you would have no way to log in") {
		t.Fatalf("own last way in: %d", code)
	}
	// Another admin may.
	zoe := e.passwordLogin(t, "zoe")
	if code, _, _ := e.post(t, zoe, "/admin/users/ann/unlink", url.Values{"issuer": {e.idp.URL}}); code != http.StatusSeeOther {
		t.Fatalf("another admin unlinking ann: %d", code)
	}
}
