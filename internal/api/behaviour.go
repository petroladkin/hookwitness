package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

// behaviourJSON is an endpoint's behaviour: its stored configuration with
// each secret described instead of stored (never a value, sealed or hashed).
type behaviourJSON struct {
	Default emulate.Reply   `json:"default"`
	Rules   []emulate.Rule  `json:"rules"`
	Checks  []emulate.Check `json:"checks"`
	Vars    []emulate.Var   `json:"vars"`
	Secrets []secretJSON    `json:"secrets"`
}

// behaviourIn is what a PUT replaces. Secrets are managed on their own
// route; a "secrets" list sent back as read is accepted and ignored.
type behaviourIn struct {
	Default emulate.Reply   `json:"default"`
	Rules   []emulate.Rule  `json:"rules"`
	Checks  []emulate.Check `json:"checks"`
	Vars    []emulate.Var   `json:"vars"`
	Secrets json.RawMessage `json:"secrets"`
}

// secretJSON describes a secret.
type secretJSON struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"` // key (signing key) or token (token or password)
	Fingerprint string   `json:"fingerprint"`
	Standard    bool     `json:"standard,omitempty"` // a "whsec_" key, usable by standard checks
	UsedBy      []string `json:"used_by"`
}

func secretView(c emulate.Config, s emulate.Secret) secretJSON {
	used := c.SecretUsers(s.Name)
	if used == nil {
		used = []string{}
	}
	return secretJSON{Name: s.Name, Kind: s.Kind, Fingerprint: s.Fingerprint, Standard: s.Standard, UsedBy: used}
}

func behaviourView(c emulate.Config) behaviourJSON {
	v := behaviourJSON{Default: c.Default, Rules: c.Rules, Checks: c.Checks, Vars: c.Vars, Secrets: []secretJSON{}}
	if v.Rules == nil {
		v.Rules = []emulate.Rule{}
	}
	if v.Checks == nil {
		v.Checks = []emulate.Check{}
	}
	if v.Vars == nil {
		v.Vars = []emulate.Var{}
	}
	for _, s := range c.Secrets {
		v.Secrets = append(v.Secrets, secretView(c, s))
	}
	return v
}

func etag(stored string) string { return `"` + emulate.Version(stored) + `"` }

func (h *Handler) getBehaviour(w http.ResponseWriter, r *http.Request) {
	ep, ok := h.endpoint(w, r)
	if !ok {
		return
	}
	cfg, err := emulate.Parse(ep.Config)
	if err != nil {
		h.fail(w, "behaviour", err)
		return
	}
	w.Header().Set("ETag", etag(ep.Config))
	writeJSON(w, http.StatusOK, behaviourView(cfg))
}

func (h *Handler) putBehaviour(w http.ResponseWriter, r *http.Request) {
	ep, ok := h.endpoint(w, r)
	if !ok {
		return
	}
	match := strings.TrimPrefix(strings.TrimSpace(r.Header.Get("If-Match")), "W/")
	if match == "" {
		WriteError(w, http.StatusPreconditionRequired, "version_required",
			"send the ETag of the behaviour you changed as If-Match (GET it first)", "")
		return
	}
	if match != etag(ep.Config) {
		h.stale(w)
		return
	}
	var in behaviourIn
	if !decode(w, r, &in, false) {
		return
	}
	stored, err := emulate.Parse(ep.Config)
	if err != nil {
		h.fail(w, "behaviour", err)
		return
	}
	cfg := emulate.Config{Default: in.Default, Rules: in.Rules, Checks: in.Checks, Vars: in.Vars, Secrets: stored.Secrets}
	if cfg.Default.Status == 0 {
		cfg.Default.Status = 200 // as stored configurations read
	}
	if err := cfg.Validate(h.o.MaxWait); err != nil {
		WriteError(w, http.StatusUnprocessableEntity, "invalid", err.Error(), fieldOf(err))
		return
	}
	saved, err := h.store.SetConfigIf(r.Context(), ep.ID, ep.Config, cfg.String())
	if err != nil {
		h.fail(w, "save behaviour", err)
		return
	}
	if !saved {
		h.stale(w)
		return
	}
	h.log.Info("endpoint behaviour changed", "path", ep.Path, "via", "api")
	w.Header().Set("ETag", etag(cfg.String()))
	writeJSON(w, http.StatusOK, behaviourView(cfg))
}

func (h *Handler) stale(w http.ResponseWriter) {
	WriteError(w, http.StatusPreconditionFailed, "stale_version",
		"the behaviour was changed after you read it; GET it again and reapply your change", "")
}

// fieldPrefixes map the start of a validation message to its JSON path.
var fieldPrefixes = regexp.MustCompile(`^(rule|check) (\d+)|^default reply|^variable|^secret`)

// fieldOf names the part of the behaviour a validation error is about.
func fieldOf(err error) string {
	m := fieldPrefixes.FindStringSubmatch(err.Error())
	switch {
	case m == nil:
		return ""
	case m[1] != "":
		n, _ := strconv.Atoi(m[2])
		return fmt.Sprintf("%ss[%d]", m[1], n-1)
	case m[0] == "default reply":
		return "default"
	case m[0] == "variable":
		return "vars"
	}
	return "secrets"
}

// updateConfig applies change to an endpoint's stored configuration and
// saves it, again from a fresh read if another client saved meanwhile.
// change answers its own errors and returns false to stop.
func (h *Handler) updateConfig(w http.ResponseWriter, r *http.Request, id int64, change func(*emulate.Config) bool) (emulate.Config, bool) {
	for range 5 {
		ep, err := h.store.Endpoint(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			WriteError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no endpoint %d", id), "")
			return emulate.Config{}, false
		}
		if err != nil {
			h.fail(w, "behaviour", err)
			return emulate.Config{}, false
		}
		cfg, err := emulate.Parse(ep.Config)
		if err != nil {
			h.fail(w, "behaviour", err)
			return emulate.Config{}, false
		}
		if !change(&cfg) {
			return emulate.Config{}, false
		}
		saved, err := h.store.SetConfigIf(r.Context(), id, ep.Config, cfg.String())
		if err != nil {
			h.fail(w, "save behaviour", err)
			return emulate.Config{}, false
		}
		if saved {
			h.log.Info("endpoint secrets changed", "path", ep.Path, "via", "api")
			return cfg, true
		}
	}
	WriteError(w, http.StatusConflict, "conflict", "the behaviour keeps changing under this call; try again", "")
	return emulate.Config{}, false
}

func (h *Handler) putSecret(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		WriteError(w, http.StatusNotFound, "not_found", "no endpoint "+r.PathValue("id"), "")
		return
	}
	name := r.PathValue("name")
	var in struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if !decode(w, r, &in, false) {
		return
	}
	if in.Kind != "" && in.Kind != emulate.SecretKey && in.Kind != emulate.SecretToken {
		WriteError(w, http.StatusUnprocessableEntity, "invalid", `kind is "key" (a signing key) or "token" (a token or password)`, "kind")
		return
	}
	var sealed emulate.Secret // sealed once: hashing a token is deliberately slow
	cfg, ok := h.updateConfig(w, r, id, func(cfg *emulate.Config) bool {
		i := slices.IndexFunc(cfg.Secrets, func(s emulate.Secret) bool { return s.Name == name })
		kind := in.Kind
		if i >= 0 {
			// Replacing keeps the kind: checks refer to the secret as it is.
			if kind != "" && kind != cfg.Secrets[i].Kind {
				WriteError(w, http.StatusUnprocessableEntity, "invalid", fmt.Sprintf(
					"%s is a %s; delete it and create it again to change the kind", name, kindName(cfg.Secrets[i].Kind)), "kind")
				return false
			}
			kind = cfg.Secrets[i].Kind
		} else if kind == "" {
			WriteError(w, http.StatusUnprocessableEntity, "invalid", `a new secret needs its kind: "key" or "token"`, "kind")
			return false
		}
		if sealed.Kind != kind {
			var err error
			if sealed, err = emulate.SealSecret(name, kind, in.Value, h.o.Keys); err != nil {
				WriteError(w, http.StatusUnprocessableEntity, "invalid", err.Error(), "value")
				return false
			}
		}
		if i >= 0 {
			cfg.Secrets[i] = sealed
		} else {
			cfg.Secrets = append(cfg.Secrets, sealed)
		}
		if err := cfg.Validate(h.o.MaxWait); err != nil {
			field := fieldOf(err)
			if strings.HasPrefix(err.Error(), "secret ") {
				field = "name"
			}
			WriteError(w, http.StatusUnprocessableEntity, "invalid", err.Error(), field)
			return false
		}
		return true
	})
	if !ok {
		return
	}
	s, _ := cfg.SecretByName(name)
	writeJSON(w, http.StatusOK, secretView(cfg, s))
}

func kindName(kind string) string {
	if kind == emulate.SecretToken {
		return "token or password"
	}
	return "signing key"
}

func (h *Handler) deleteSecret(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		WriteError(w, http.StatusNotFound, "not_found", "no endpoint "+r.PathValue("id"), "")
		return
	}
	name := r.PathValue("name")
	_, ok = h.updateConfig(w, r, id, func(cfg *emulate.Config) bool {
		i := slices.IndexFunc(cfg.Secrets, func(s emulate.Secret) bool { return s.Name == name })
		if i < 0 {
			WriteError(w, http.StatusNotFound, "not_found", "no secret "+name, "")
			return false
		}
		if users := cfg.SecretUsers(name); len(users) > 0 {
			WriteError(w, http.StatusConflict, "secret_in_use",
				fmt.Sprintf("%s is used by %s; change or delete those checks first", name, strings.Join(users, ", ")), "")
			return false
		}
		cfg.Secrets = slices.Delete(cfg.Secrets, i, i+1)
		return true
	})
	if ok {
		w.WriteHeader(http.StatusNoContent)
	}
}
