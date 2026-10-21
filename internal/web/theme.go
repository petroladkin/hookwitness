package web

import (
	"net/http"

	"github.com/petroladkin/hookwitness/internal/access"
)

// The theme is a choice of this browser, not of a user: a cookie holding
// "light" or "dark" (none: follow the system). The server writes it into
// <html data-theme> so the first paint already has it, with no script.

const themeCookieName = "hw_theme"

// theme is the request's theme: "light", "dark" or "" (auto).
func theme(r *http.Request) string {
	c, err := r.Cookie(themeCookieName)
	if err != nil {
		return ""
	}
	switch c.Value {
	case "light", "dark":
		return c.Value
	}
	return ""
}

// colorScheme is the color-scheme meta for a theme.
func colorScheme(theme string) string {
	if theme == "" {
		return "light dark"
	}
	return theme
}

// setTheme keeps the choice posted from the masthead and goes back to the
// page it was made on.
func (h *Handler) setTheme(w http.ResponseWriter, r *http.Request) {
	c := &http.Cookie{
		Name:     themeCookieName,
		Path:     access.CookiePath(h.prefix),
		HttpOnly: true,
		Secure:   h.secure(r),
		SameSite: http.SameSiteStrictMode,
	}
	switch v := r.PostFormValue("theme"); v {
	case "light", "dark":
		c.Value, c.MaxAge = v, 365*24*3600
	case "auto":
		c.MaxAge = -1
	default:
		http.Error(w, "The theme is auto, light or dark.", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, c)
	http.Redirect(w, r, h.back(r, h.prefix+"/"), http.StatusSeeOther)
}

// themes are the switch's three choices.
var themes = []struct{ value, theme, label, title string }{
	{"auto", "", "Auto", "Follow the system's light or dark setting"},
	{"light", "light", "Light", "Always light"},
	{"dark", "dark", "Dark", "Always dark"},
}
