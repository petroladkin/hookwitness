package web

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/store"
)

// The masthead is the same on every page: the sections as tabs, the catch
// address and a menu; the register picker lives in the register's name and
// the build line at the foot of every page.
func TestMasthead(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, _ := e.st.CreateEndpoint(ctx, "/orders")
	e.st.CreateEndpoint(ctx, "/refunds")
	e.add(t, a.ID, "{}", nil)
	e.add(t, a.ID, "{}", nil)
	reg := fmt.Sprintf("/admin/e/%d", a.ID)

	for _, path := range []string{"/admin/", reg, reg + "/behaviour", "/admin/e/unmatched"} {
		page := e.do("GET", path, nil).Body.String()
		mast := page[strings.Index(page, `<header class="mast">`):strings.Index(page, `</header>`)]
		for _, want := range []string{
			`<a href="/admin/" aria-current="page">Endpoints</a>`,
			`<span class="addr">example.com</span>`,
			// Login is off: the menu holds the theme only.
			`<details class="menu acct" data-menu><summary>Theme</summary>`,
			`name="theme" value="dark"`,
		} {
			if !strings.Contains(mast, want) {
				t.Fatalf("%s: masthead lacks %s:\n%s", path, want, mast)
			}
		}
		for _, not := range []string{"<select", "Log out", ">Users<", `class="menu pick"`} {
			if strings.Contains(mast, not) {
				t.Fatalf("%s: masthead has %s:\n%s", path, not, mast)
			}
		}
		if !strings.Contains(page, `<footer class="build">Hookwitness dev (none)</footer></div></body>`) {
			t.Fatalf("%s: no build line at the foot", path)
		}
	}

	// The register's name is the picker: every register with its count, the
	// current one marked, no groups while all are on, the way to all.
	page := e.do("GET", reg, nil).Body.String()
	for _, want := range []string{
		`<summary class="reg-name">/orders</summary>`,
		`<a class="menu-item" href="/admin/e/1" aria-current="page"><span>/orders</span><span class="n">2</span></a>`,
		`<a class="menu-item" href="/admin/e/2" aria-current="false"><span>/refunds</span><span class="n">0</span></a>`,
		`<a class="menu-item apart" href="/admin/e/unmatched" aria-current="false"><span>Unmatched</span><span class="n">0</span></a>`,
		`<a class="menu-item apart" href="/admin/"><span>All endpoints</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("picker lacks %s:\n%s", want, page)
		}
	}
	if strings.Contains(page, `class="menu-label">On<`) {
		t.Fatal("groups while every endpoint is on")
	}
	if page := e.do("GET", "/admin/e/unmatched", nil).Body.String(); !strings.Contains(page, `<summary class="reg-name">Unmatched</summary>`) ||
		!strings.Contains(page, `href="/admin/e/unmatched" aria-current="page"`) {
		t.Fatalf("Unmatched picker:\n%s", page)
	}
}

// Logged in, the menu carries the name, the account, the theme and log
// out; the Users tab is an admin's and marked on the users page.
func TestMastheadLoggedIn(t *testing.T) {
	e := newAccessEnv(t, "")
	e.addUser(t, "ann", store.RoleAdmin, adminPW)
	e.addUser(t, "bob", store.RoleUser, adminPW)

	ann := e.login(t, "ann", adminPW)
	page := e.req("GET", "/admin/users", ann, "", nil).Body.String()
	for _, want := range []string{
		`<a href="/admin/" aria-current="false">Endpoints</a>`,
		`<a href="/admin/users" aria-current="page">Users</a>`,
		`<summary>ann</summary>`,
		`Admin: manages endpoints and users`,
		`<a class="menu-item" href="/admin/account">Account and tokens</a>`,
		`<button class="menu-item" type="submit">Log out</button>`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("admin's masthead lacks %s:\n%s", want, page)
		}
	}
	if page := e.req("GET", "/admin/account", ann, "", nil).Body.String(); strings.Contains(page, `aria-current="page">Endpoints`) || strings.Contains(page, `aria-current="page">Users`) {
		t.Fatal("a tab marked on the account page")
	}

	bob := e.login(t, "bob", adminPW)
	page = e.req("GET", "/admin/", bob, "", nil).Body.String()
	if strings.Contains(page, `href="/admin/users"`) || !strings.Contains(page, `User: manages endpoints`) {
		t.Fatalf("user's masthead:\n%s", page)
	}

	// The login page has no masthead controls and no build line.
	page = e.req("GET", "/admin/login", "", "", nil).Body.String()
	if strings.Contains(page, `class="sections"`) || strings.Contains(page, `class="build"`) {
		t.Fatalf("login page:\n%s", page)
	}
}
