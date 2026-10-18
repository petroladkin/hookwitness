package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// TestTheme: the theme chosen in the masthead is kept in a cookie of this
// browser and applied by the server, on the first paint of every page.
func TestTheme(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.st.CreateEndpoint(t.Context(), "/orders")
	behaviour := "/admin/e/" + strconv.FormatInt(ep.ID, 10) + "/behaviour"

	// No cookie: auto, as before; the switch offers the three, Auto pressed.
	page := e.do("GET", "/admin/", nil).Body.String()
	if strings.Contains(page, "data-theme=") || !strings.Contains(page, `<meta name="color-scheme" content="light dark">`) {
		t.Fatalf("auto page:\n%s", page)
	}
	for _, want := range []string{
		`<form class="theme" method="post" action="/admin/theme"`,
		`name="theme" value="auto" aria-pressed="true"`,
		`name="theme" value="light" aria-pressed="false"`,
		`name="theme" value="dark" aria-pressed="false"`,
		`<input type="hidden" name="next" value="/admin/">`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("the switch lacks %s:\n%s", want, page)
		}
	}

	// Choosing sets the cookie and goes back to the page it came from.
	rec := e.do("POST", "/admin/theme", url.Values{"theme": {"dark"}, "next": {"/admin/e/unmatched"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/e/unmatched" {
		t.Fatalf("choose dark: %d %v", rec.Code, rec.Header())
	}
	c := themeCookie(t, rec)
	if c.Value != "dark" || c.Path != "/admin/" || c.SameSite != http.SameSiteStrictMode || !c.HttpOnly ||
		c.MaxAge < 364*24*3600 {
		t.Fatalf("cookie: %+v", c)
	}

	for _, theme := range []string{"dark", "light"} {
		for _, path := range []string{"/admin/", "/admin/e/unmatched", behaviour, "/admin/login"} {
			page := e.do("GET", path, nil, "Cookie", "hw_theme="+theme).Body.String()
			if !strings.Contains(page, `<html lang="en" data-theme="`+theme+`">`) ||
				!strings.Contains(page, `<meta name="color-scheme" content="`+theme+`">`) {
				t.Fatalf("%s with %s:\n%.600s", path, theme, page)
			}
		}
		page := e.do("GET", "/admin/", nil, "Cookie", "hw_theme="+theme).Body.String()
		if !strings.Contains(page, `value="`+theme+`" aria-pressed="true"`) {
			t.Fatalf("%s not pressed:\n%s", theme, page)
		}
	}

	// Auto clears it.
	rec = e.do("POST", "/admin/theme", url.Values{"theme": {"auto"}}, "Cookie", "hw_theme=dark")
	if c := themeCookie(t, rec); c.MaxAge >= 0 || rec.Header().Get("Location") != "/admin/" {
		t.Fatalf("auto: %d %v", rec.Code, rec.Header())
	}

	// A cookie of another value is auto; a bad choice is refused; "next"
	// outside the admin is ignored.
	if page := e.do("GET", "/admin/", nil, "Cookie", `hw_theme=x"><script>`).Body.String(); strings.Contains(page, "data-theme=") {
		t.Fatal("an unknown cookie value reached the page")
	}
	if rec := e.do("POST", "/admin/theme", url.Values{"theme": {"sepia"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad theme: %d", rec.Code)
	}
	for _, next := range []string{"https://evil.example/", "//evil.example/admin/", "/elsewhere"} {
		rec := e.do("POST", "/admin/theme", url.Values{"theme": {"light"}, "next": {next}})
		if rec.Header().Get("Location") != "/admin/" {
			t.Fatalf("next %q: %v", next, rec.Header())
		}
	}

	// A form posted from another site is refused like every other.
	if rec := e.do("POST", "/admin/theme", url.Values{"theme": {"dark"}}, "Sec-Fetch-Site", "cross-site"); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site: %d", rec.Code)
	}
}

// TestThemeCookieSecure: the cookie is Secure when the admin is behind TLS.
func TestThemeCookieSecure(t *testing.T) {
	e := newEnvWith(t, Options{Prefix: "/admin", PublicURL: "https://hooks.example.test", Hosts: []string{"example.com"}})
	rec := e.do("POST", "/admin/theme", url.Values{"theme": {"light"}})
	if c := themeCookie(t, rec); !c.Secure {
		t.Fatalf("cookie: %+v", c)
	}
}

func themeCookie(t *testing.T, rec interface{ Result() *http.Response }) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hw_theme" {
			return c
		}
	}
	t.Fatal("no hw_theme cookie")
	return nil
}
