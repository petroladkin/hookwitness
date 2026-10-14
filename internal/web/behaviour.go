package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/secret"
	"github.com/petroladkin/hookwitness/internal/store"
)

// behaviourView is an endpoint's behaviour page: one tab of lists, and the
// editor dialog when one is open.
type behaviourView struct {
	ID      int64
	Path    string
	Base    string // the endpoint's register URL
	Counter int64
	Config  emulate.Config // as stored
	MaxWait time.Duration
	Version string // of the stored configuration; every form sends it back
	Tab     string // checks, rules, default or vars
	Saved   string // the section the last edit saved ("rule-2", "counter", ...)
	Error   string // the last edit was refused: why
	NoDraft string // a schema draft was asked for and none could be made: why
	Editor  *editor
}

// editor is the dialog that creates or edits one item.
type editor struct {
	Kind   string // rule, check, var or default
	I      string // the item's index, or "new"
	Rule   emulate.Rule
	Check  emulate.Check
	Var    emulate.Var
	Secret emulate.Secret // never with a value: secrets are write-only
	Schema emulate.Schema
	Draft  *draftView    // the schema was drafted from requests
	Reply  emulate.Reply // the default reply
}

// behaviourTabs in the order a request meets them.
var behaviourTabs = []string{"checks", "rules", "default", "vars", "secrets", "schemas"}

// tabOf is the tab that holds a section ("rule-2", "checks", "var-new",
// ...); rules when unknown.
func tabOf(section string) string {
	switch {
	case strings.HasPrefix(section, "check"):
		return "checks"
	case strings.HasPrefix(section, "var"):
		return "vars"
	case strings.HasPrefix(section, "secret"):
		return "secrets"
	case strings.HasPrefix(section, "schema"):
		return "schemas"
	case section == "default":
		return "default"
	}
	return "rules"
}

// openEditor is the editor a URL names ("rule-2", "rule-new", "default"),
// or nil.
func openEditor(cfg emulate.Config, which string) *editor {
	if which == "default" {
		return &editor{Kind: "default", Reply: cfg.Default}
	}
	kind, i, _ := strings.Cut(which, "-")
	e := &editor{Kind: kind, I: i}
	n, err := strconv.Atoi(i)
	if i != "new" && (err != nil || n < 0) {
		return nil
	}
	switch kind {
	case "rule":
		if i == "new" {
			e.Rule = newRule
		} else if n < len(cfg.Rules) {
			e.Rule = cfg.Rules[n]
		} else {
			return nil
		}
	case "check":
		if i == "new" {
			e.Check = newCheck
		} else if n < len(cfg.Checks) {
			e.Check = cfg.Checks[n]
		} else {
			return nil
		}
	case "var":
		if i != "new" && n >= len(cfg.Vars) {
			return nil
		} else if i != "new" {
			e.Var = cfg.Vars[n]
		}
	case "secret":
		if i == "new" {
			e.Secret = emulate.Secret{Kind: emulate.SecretKey}
		} else if n < len(cfg.Secrets) {
			e.Secret = cfg.Secrets[n]
		} else {
			return nil
		}
	case "schema":
		if i != "new" && n >= len(cfg.Schemas) {
			return nil
		} else if i != "new" {
			e.Schema = cfg.Schemas[n]
		}
	default:
		return nil
	}
	return e
}

// What a new rule or check starts as.
var (
	newRule  = emulate.Rule{When: emulate.Condition{Kind: emulate.WhenEvery, N: 2}, Do: emulate.ActReply, Reply: emulate.Reply{Status: 500}}
	newCheck = emulate.Check{Match: emulate.MatchPresent}
)

// configVersion identifies a stored configuration, so an edit made on a page
// loaded before another change is refused instead of hitting the wrong rule.
func configVersion(stored string) string { return emulate.Version(stored) }

func (h *Handler) behaviourPage(w http.ResponseWriter, r *http.Request) {
	id, ok := regID(r)
	if !ok || id == store.Unmatched {
		http.NotFound(w, r)
		return
	}
	ep, err := h.store.Endpoint(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, r, "This endpoint does not exist (any more).")
		return
	}
	if err != nil {
		h.fail(w, "behaviour", err)
		return
	}
	cfg, err := emulate.Parse(ep.Config)
	if err != nil {
		h.fail(w, "behaviour", err)
		return
	}
	q := r.URL.Query()
	v := h.behaviourView(ep, cfg)
	if q.Get("edit") == "schema-draft" {
		ed, why, err := h.openDraft(r.Context(), ep, cfg, q)
		if err != nil {
			h.fail(w, "schema draft", err)
			return
		}
		v.Editor, v.NoDraft = ed, why
	} else if q.Has("edit") {
		v.Editor = openEditor(cfg, q.Get("edit"))
		// "Add a check that uses it" from a secret: start from that secret.
		if e := v.Editor; e != nil && e.Kind == "check" && e.I == "new" {
			if sc, ok := cfg.SecretByName(q.Get("secret")); ok {
				e.Check = checkFor(sc)
			}
			if name := q.Get("schema"); name != "" {
				e.Check = emulate.Check{Kind: emulate.KindSchema, Schema: name}
			}
		}
	}
	v.Tab = q.Get("tab")
	if !slices.Contains(behaviourTabs, v.Tab) {
		v.Tab = "rules"
		if v.Editor != nil {
			v.Tab = tabOf(q.Get("edit"))
		}
	}
	if v.Editor == nil {
		v.Saved = q.Get("saved")
	}
	h.showBehaviour(w, r, http.StatusOK, v)
}

func (h *Handler) showBehaviour(w http.ResponseWriter, r *http.Request, status int, v behaviourView) {
	nav, err := h.nav(r, v.ID, false)
	if err != nil {
		h.fail(w, "behaviour", err)
		return
	}
	h.render(w, r, status, behaviourPage(nav, v))
}

func (h *Handler) behaviourView(ep store.Endpoint, cfg emulate.Config) behaviourView {
	return behaviourView{
		ID: ep.ID, Path: ep.Path, Base: h.regPath(ep.ID), Counter: ep.Counter,
		Config: cfg, MaxWait: h.maxWait, Version: configVersion(ep.Config),
	}
}

// editBehaviour applies one edit (the form's "op") to the endpoint's
// behaviour, validates the whole and saves it. A refused edit shows the page
// again, with the editor open on what was typed and why it was refused.
func (h *Handler) editBehaviour(w http.ResponseWriter, r *http.Request) {
	id, ok := regID(r)
	if !ok || id == store.Unmatched {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	ep, err := h.store.Endpoint(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, "behaviour", err)
		return
	}
	stored, err := emulate.Parse(ep.Config)
	if err != nil {
		h.fail(w, "behaviour", err)
		return
	}
	refuse := func(status int, section, msg string, ed *editor) {
		v := h.behaviourView(ep, stored)
		v.Tab, v.Error, v.Editor = tabOf(section), msg, ed
		h.showBehaviour(w, r, status, v)
	}

	op := r.PostFormValue("op")
	if op != "counter-reset" && r.PostFormValue("v") != configVersion(ep.Config) {
		refuse(http.StatusConflict, op, "the behaviour was changed elsewhere (another tab or client) after this page was loaded, so nothing was saved; "+
			"this is the current version, make the change again", nil)
		return
	}
	if op == "counter-reset" {
		if err := h.store.ResetCounter(ctx, id); err != nil {
			h.fail(w, "reset counter", err)
			return
		}
		http.Redirect(w, r, savedURL(h.regPath(id), "counter"), http.StatusSeeOther)
		return
	}
	cfg, _ := emulate.Parse(ep.Config) // a copy to edit
	section, err := applyEdit(&cfg, op, r, h.keys)
	if err == nil {
		err = cfg.Validate(h.maxWait)
	}
	if err != nil {
		var ed *editor
		if strings.HasSuffix(op, "-save") || op == "default" {
			// Reopen the editor on what was typed (applied to the copy).
			if ed = openEditor(cfg, section); ed != nil && ed.Kind != "default" {
				ed.I = r.PostFormValue("i")
			}
			// A refused drafted schema keeps its offer to map the value.
			if ed != nil && ed.Kind == "schema" && r.PostFormValue("draft_from") != "" {
				n, _ := strconv.Atoi(r.PostFormValue("map_check"))
				ed.Draft = &draftView{From: r.PostFormValue("draft_from"), MapCheck: n,
					MapLabel: r.PostFormValue("map_label"), MapValue: r.PostFormValue("map_value")}
			}
			// A refused replacement shows the secret as it is still stored.
			if ed != nil && ed.Kind == "secret" {
				if n, err := strconv.Atoi(ed.I); err == nil && n < len(stored.Secrets) {
					ed.Secret = stored.Secrets[n]
				}
			}
		}
		refuse(http.StatusUnprocessableEntity, section, err.Error(), ed)
		return
	}
	if err := h.store.SetConfig(ctx, id, cfg.String()); err != nil {
		h.fail(w, "save behaviour", err)
		return
	}
	h.log.Info("endpoint behaviour changed", "path", ep.Path, "op", op)
	http.Redirect(w, r, savedURL(h.regPath(id), section), http.StatusSeeOther)
}

// savedURL shows the tab of a saved section and says so.
func savedURL(base, section string) string {
	return base + "/behaviour?tab=" + tabOf(section) + "&saved=" + url.QueryEscape(section)
}

// applyEdit changes cfg as the form asks; it returns the section edited
// ("rule-2"; the list, "rules", after a delete).
func applyEdit(cfg *emulate.Config, op string, r *http.Request, keys *secret.Key) (string, error) {
	kind, act, _ := strings.Cut(op, "-")
	// at is the item the form names: an index, or "new" (one past the end)
	// where new is allowed.
	at := func(n int, newOK bool) (int, error) {
		s := r.PostFormValue("i")
		if s == "new" && newOK {
			return n, nil
		}
		i, err := strconv.Atoi(s)
		if err != nil || i < 0 || i >= n {
			return 0, fmt.Errorf("that %s no longer exists; reload the page", kind)
		}
		return i, nil
	}
	switch op {
	case "default":
		reply, err := formReply(r, "")
		cfg.Default = reply
		return "default", err
	case "rule-save", "rule-up", "rule-down", "rule-del":
		i, err := at(len(cfg.Rules), act == "save")
		if err != nil {
			return "rules", err
		}
		switch act {
		case "save":
			rule, err := formRule(r)
			if i == len(cfg.Rules) {
				cfg.Rules = append(cfg.Rules, rule)
			} else {
				cfg.Rules[i] = rule
			}
			return fmt.Sprintf("rule-%d", i), err
		case "up":
			if i > 0 {
				cfg.Rules[i-1], cfg.Rules[i] = cfg.Rules[i], cfg.Rules[i-1]
				return fmt.Sprintf("rule-%d", i-1), nil
			}
			return fmt.Sprintf("rule-%d", i), nil
		case "down":
			if i < len(cfg.Rules)-1 {
				cfg.Rules[i+1], cfg.Rules[i] = cfg.Rules[i], cfg.Rules[i+1]
				return fmt.Sprintf("rule-%d", i+1), nil
			}
			return fmt.Sprintf("rule-%d", i), nil
		}
		cfg.Rules = slices.Delete(cfg.Rules, i, i+1)
		return "rules", nil
	case "check-save", "check-del":
		i, err := at(len(cfg.Checks), act == "save")
		if err != nil {
			return "checks", err
		}
		if act == "del" {
			cfg.Checks = slices.Delete(cfg.Checks, i, i+1)
			return "checks", nil
		}
		ch, err := formCheck(r)
		if r.PostFormValue("own_error") == "on" {
			var reply emulate.Reply
			reply, err = formReply(r, "error_")
			ch.Error = &reply
		}
		if i == len(cfg.Checks) {
			cfg.Checks = append(cfg.Checks, ch)
		} else {
			cfg.Checks[i] = ch
		}
		return fmt.Sprintf("check-%d", i), err
	case "secret-save", "secret-del":
		i, err := at(len(cfg.Secrets), act == "save")
		if err != nil {
			return "secrets", err
		}
		if act == "del" {
			if users := cfg.SecretUsers(cfg.Secrets[i].Name); len(users) > 0 {
				return "secrets", fmt.Errorf("%s is used by %s; change or delete those checks first", cfg.Secrets[i].Name, strings.Join(users, ", "))
			}
			cfg.Secrets = slices.Delete(cfg.Secrets, i, i+1)
			return "secrets", nil
		}
		s, err := formSecret(r, keys)
		if i < len(cfg.Secrets) {
			// Replacing keeps the name and the kind: checks refer to them.
			s.Name, s.Kind = cfg.Secrets[i].Name, cfg.Secrets[i].Kind
			if err == nil {
				s, err = sealSecret(s.Name, s.Kind, r.PostFormValue("value"), keys)
			}
			cfg.Secrets[i] = s
		} else {
			cfg.Secrets = append(cfg.Secrets, s)
		}
		return fmt.Sprintf("secret-%d", i), err
	case "schema-save", "schema-del":
		i, err := at(len(cfg.Schemas), act == "save")
		if err != nil {
			return "schemas", err
		}
		if act == "del" {
			if users := cfg.SchemaUsers(cfg.Schemas[i].Name); len(users) > 0 {
				return "schemas", fmt.Errorf("%s is used by %s; change or delete those checks first", cfg.Schemas[i].Name, strings.Join(users, ", "))
			}
			cfg.Schemas = slices.Delete(cfg.Schemas, i, i+1)
			return "schemas", nil
		}
		sc, err := formSchema(r)
		if r.PostFormValue("map") == "on" && err == nil {
			// A drafted schema mapped in the discriminator check it came from.
			n, _ := strconv.Atoi(r.PostFormValue("map_check"))
			cfg.MapValue(n, r.PostFormValue("map_value"), sc.Name)
		}
		if i == len(cfg.Schemas) {
			cfg.Schemas = append(cfg.Schemas, sc)
		} else {
			if old := cfg.Schemas[i].Name; old != sc.Name && len(cfg.SchemaUsers(old)) > 0 && err == nil {
				err = fmt.Errorf("%s is used by %s; a used schema keeps its name", old, strings.Join(cfg.SchemaUsers(old), ", "))
			}
			cfg.Schemas[i] = sc
		}
		return fmt.Sprintf("schema-%d", i), err
	case "var-save", "var-del":
		i, err := at(len(cfg.Vars), act == "save")
		if err != nil {
			return "vars", err
		}
		if act == "del" {
			cfg.Vars = slices.Delete(cfg.Vars, i, i+1)
			return "vars", nil
		}
		v := emulate.Var{Name: strings.TrimSpace(r.PostFormValue("name")), Value: r.PostFormValue("value")}
		if i == len(cfg.Vars) {
			cfg.Vars = append(cfg.Vars, v)
		} else {
			cfg.Vars[i] = v
		}
		return fmt.Sprintf("var-%d", i), nil
	}
	return "", fmt.Errorf("unknown edit %q", op)
}

// formCheck reads a check editor: the fields its kind uses.
func formCheck(r *http.Request) (emulate.Check, error) {
	v := func(name string) string { return strings.TrimSpace(r.PostFormValue(name)) }
	ch := emulate.Check{Kind: v("kind")}
	if ch.Kind == emulate.KindHeader {
		ch.Kind = "" // stored as before authorization checks
	}
	if !ch.GivesVerdict() {
		ch.Header, ch.Match, ch.Value = v("header"), r.PostFormValue("match"), r.PostFormValue("value")
		return ch, nil
	}
	ch.Enforce = r.PostFormValue("enforce") == "on"
	ch.Secret = v("secret")
	var err error
	switch ch.Kind {
	case emulate.KindHMAC:
		ch.Header, ch.Algo, ch.Encoding = v("header"), v("algo"), v("encoding")
		ch.Prefix, ch.Content = r.PostFormValue("prefix"), r.PostFormValue("content")
	case emulate.KindBasic:
		ch.User = v("user")
	case emulate.KindSchema:
		ch.Secret = ""
		if from := v("select_from"); from != "" {
			ch.Select = &emulate.Select{From: from, Key: v("select_key"), Default: emulate.Names(r.PostForm["select_default"])}
			ch.Select.Map, err = parseMapping(r.PostFormValue("select_map"))
		} else if names := r.PostForm["schema"]; len(names) == 1 {
			ch.Schema = names[0]
		} else if len(names) > 1 {
			ch.Schemas = names
		}
	case emulate.KindStripe, emulate.KindStandard:
		ch.IgnoreTimestamp = r.PostFormValue("ignore_timestamp") == "on"
		if s := v("tolerance_s"); s != "" {
			ch.ToleranceS, err = strconv.ParseInt(s, 10, 64)
			if err != nil {
				err = errors.New("the tolerance must be a whole number of seconds")
			}
		}
	}
	return ch, err
}

// maxSchema bounds a schema pasted or uploaded.
const maxSchema = 1 << 20

// formSchema reads a schema editor: the name, and the text pasted or the
// file uploaded (a file wins).
func formSchema(r *http.Request) (emulate.Schema, error) {
	sc := emulate.Schema{Name: strings.TrimSpace(r.PostFormValue("name")), Text: r.PostFormValue("text")}
	if f, hdr, err := r.FormFile("file"); err == nil {
		defer f.Close()
		if hdr.Size > maxSchema {
			return sc, fmt.Errorf("the file is over %d KiB", maxSchema>>10)
		}
		b, err := io.ReadAll(io.LimitReader(f, maxSchema+1))
		if err != nil {
			return sc, fmt.Errorf("the file could not be read: %v", err)
		}
		if len(b) > 0 {
			sc.Text = string(b)
		}
	}
	if strings.TrimSpace(sc.Text) == "" {
		return sc, errors.New("paste the schema or choose a file")
	}
	if len(sc.Text) > maxSchema {
		return sc, fmt.Errorf("the schema is over %d KiB", maxSchema>>10)
	}
	return sc, nil
}

// parseMapping reads "value = Schema" lines.
func parseMapping(text string) ([]emulate.Mapping, error) {
	var out []emulate.Mapping
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		i := strings.LastIndex(line, "=")
		if i < 0 {
			return out, fmt.Errorf("mapping line %q: write it as value = SchemaName", line)
		}
		out = append(out, emulate.Mapping{Value: strings.TrimSpace(line[:i]), Schema: strings.TrimSpace(line[i+1:])})
	}
	return out, nil
}

// checkUses reports a schema among a check's alternatives (no Select).
func checkUses(ch emulate.Check, name string) bool {
	return ch.Select == nil && (ch.Schema == name || slices.Contains(ch.Schemas, name))
}

// formatMapping is the editor text of a mapping.
func formatMapping(s *emulate.Select) string {
	if s == nil {
		return ""
	}
	var b strings.Builder
	for _, m := range s.Map {
		b.WriteString(m.Value + " = " + m.Schema + "\n")
	}
	return b.String()
}

// formSecret reads a new secret and seals or hashes its value at once; the
// plain value goes no further than this request.
func formSecret(r *http.Request, keys *secret.Key) (emulate.Secret, error) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	kind := r.PostFormValue("secret_kind")
	return sealSecret(name, kind, r.PostFormValue("value"), keys)
}

func sealSecret(name, kind, value string, keys *secret.Key) (emulate.Secret, error) {
	if keys == nil {
		return emulate.SealSecret(name, kind, value, nil) // a nil *Key is not a nil Sealer
	}
	return emulate.SealSecret(name, kind, value, keys)
}

func formReply(r *http.Request, prefix string) (emulate.Reply, error) {
	v := func(name string) string { return r.PostFormValue(prefix + name) }
	reply := emulate.Reply{Body: v("body")}
	if v("body_as") == "base64" { // the exact bytes: whitespace and line breaks are not part of them
		reply.Body, reply.BodyBase64 = "", strings.Join(strings.Fields(v("body")), "")
	}
	var errs []string
	num := func(name, label string) int64 {
		s := strings.TrimSpace(v(name))
		if s == "" {
			return 0
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			errs = append(errs, label+" must be a whole number")
		}
		return n
	}
	reply.Status = int(num("status", "status"))
	reply.DelayMs = num("delay", "delay")
	reply.JitterMs = num("jitter", "jitter")
	hs, err := emulate.ParseHeaders(v("headers"))
	if err != nil {
		errs = append(errs, err.Error())
	}
	reply.Headers = hs
	if len(errs) > 0 {
		return reply, errors.New(strings.Join(errs, "; "))
	}
	return reply, nil
}

func formRule(r *http.Request) (emulate.Rule, error) {
	rule := emulate.Rule{
		When: emulate.Condition{Kind: r.PostFormValue("when")},
		Do:   r.PostFormValue("do"),
	}
	var errs []string
	switch rule.When.Kind {
	case emulate.WhenRandom:
		p, err := strconv.ParseFloat(strings.TrimSpace(r.PostFormValue("percent")), 64)
		if err != nil {
			errs = append(errs, "the percentage must be a number")
		}
		rule.When.Percent = p
	default:
		n, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("n")), 10, 64)
		if err != nil {
			errs = append(errs, "the count N must be a whole number")
		}
		rule.When.N = n
	}
	reply, err := formReply(r, "")
	if err != nil {
		errs = append(errs, err.Error())
	}
	if rule.Do == emulate.ActReply {
		rule.Reply = reply
	}
	if len(errs) > 0 {
		return rule, errors.New(strings.Join(errs, "; "))
	}
	return rule, nil
}

// Helpers for the templates.

func tabTitle(tab string) string {
	switch tab {
	case "checks":
		return "Checks"
	case "default":
		return "Default"
	case "vars":
		return "Variables"
	case "secrets":
		return "Secrets"
	case "schemas":
		return "Schemas"
	}
	return "Rules"
}

// tabCount is how many items a tab holds, "" for tabs that hold one thing.
func tabCount(c emulate.Config, tab string) string {
	switch tab {
	case "checks":
		return strconv.Itoa(len(c.Checks))
	case "rules":
		return strconv.Itoa(len(c.Rules))
	case "vars":
		return strconv.Itoa(len(c.Vars))
	case "secrets":
		return strconv.Itoa(len(c.Secrets))
	case "schemas":
		return strconv.Itoa(len(c.Schemas))
	}
	return ""
}

func editorTitle(e *editor) string {
	switch e.Kind {
	case "default":
		return "Default reply"
	case "var":
		if e.I == "new" {
			return "New variable"
		}
		return "Variable " + e.Var.Name
	case "secret":
		if e.I == "new" {
			return "New secret"
		}
		return "Replace " + e.Secret.Name
	case "schema":
		if e.I == "new" {
			return "New schema"
		}
		return "Schema " + e.Schema.Name
	}
	if e.I == "new" {
		return "New " + e.Kind
	}
	n, _ := strconv.Atoi(e.I)
	return strings.ToUpper(e.Kind[:1]) + e.Kind[1:] + " " + strconv.Itoa(n+1)
}

func editorSave(e *editor) string {
	switch e.Kind {
	case "default":
		return "Save default reply"
	case "var":
		return "Save variable"
	case "secret":
		return "Save secret"
	case "schema":
		return "Save schema"
	}
	return "Save " + e.Kind
}

func varOrName(name string) string {
	if name == "" {
		return "NAME"
	}
	return name
}

// refusalLabel says how a check refuses.
func refusalLabel(c emulate.Check) string {
	switch {
	case c.Error != nil:
		return strconv.Itoa(c.Error.Status) + " (own reply)"
	case c.Kind == emulate.KindSchema:
		return "400 listing the problems"
	case c.GivesVerdict():
		return "401"
	}
	return "400 naming the header"
}

// checkSummary describes a check in its list row, after its label.
func checkSummary(c emulate.Check) string {
	if !c.GivesVerdict() {
		return matchLabel(c) + " · else " + refusalLabel(c)
	}
	parts := []string{"secret " + c.Secret}
	if c.Kind == emulate.KindSchema {
		parts = nil
		if c.Select != nil {
			for _, m := range c.Select.Map {
				parts = append(parts, strconv.Quote(m.Value)+" → "+m.Schema)
			}
			other := "other values not checked"
			if d := c.Select.Default; len(d) > 0 {
				other = "other values → " + strings.Join(d, " or ")
			}
			parts = []string{strings.Join(parts, ", "), other}
		}
	}
	switch c.Kind {
	case emulate.KindBasic:
		parts = append(parts, "user "+c.User)
	case emulate.KindHMAC:
		parts = append(parts, c.Algo+" "+c.Encoding)
	case emulate.KindStripe, emulate.KindStandard:
		if c.IgnoreTimestamp {
			parts = append(parts, "any timestamp")
		} else {
			parts = append(parts, "tolerance "+strconv.FormatInt(tolerance(c), 10)+" s")
		}
	}
	if c.Enforce {
		parts = append(parts, "enforced · else "+refusalLabel(c))
	} else {
		parts = append(parts, "record only")
	}
	return strings.Join(parts, " · ")
}

func tolerance(c emulate.Check) int64 {
	if c.ToleranceS > 0 {
		return c.ToleranceS
	}
	return auth.DefaultTolerance
}

// kindTitle names a check kind in the editor's select.
func kindTitle(k string) string {
	switch k {
	case emulate.KindHeader:
		return "Required header"
	case emulate.KindStandard:
		return "Standard Webhooks / Svix signature"
	case emulate.KindHMAC:
		return "Generic HMAC signature"
	case emulate.KindSchema:
		return "JSON Schema"
	}
	return emulate.Check{Kind: k}.Label()
}

func checkKind(c emulate.Check) string {
	if c.Kind == "" {
		return emulate.KindHeader
	}
	return c.Kind
}

func secretKindTitle(k string) string {
	if k == emulate.SecretToken {
		return "token or password"
	}
	return "signing key"
}

// checkFor is a new check that uses a secret: a bearer check for a token, a
// GitHub signature check for a signing key; the kind can be changed.
func checkFor(sc emulate.Secret) emulate.Check {
	if sc.Kind == emulate.SecretToken {
		return emulate.Check{Kind: emulate.KindBearer, Secret: sc.Name}
	}
	return emulate.Check{Kind: emulate.KindGitHub, Secret: sc.Name}
}

// unusedSaved is the name of the secret the last edit saved when no check
// uses it yet, else "".
func unusedSaved(b behaviourView) string {
	n, ok := strings.CutPrefix(b.Saved, "secret-")
	i, err := strconv.Atoi(n)
	if !ok || err != nil || i >= len(b.Config.Secrets) || len(b.Config.SecretUsers(b.Config.Secrets[i].Name)) > 0 {
		return ""
	}
	return b.Config.Secrets[i].Name
}

// schemaUse says which checks use a schema, for its list row.
func schemaUse(c emulate.Config, name string) string {
	users := c.SchemaUsers(name)
	if len(users) == 0 {
		return "not used"
	}
	return "used by " + strings.Join(users, ", ")
}

// schemaDraft names the draft a schema declares, "2020-12" by default.
func schemaDraft(text string) string {
	var head struct {
		Schema string `json:"$schema"`
	}
	json.Unmarshal([]byte(text), &head)
	switch {
	case head.Schema == "":
		return "2020-12"
	case strings.Contains(head.Schema, "draft-07"):
		return "draft-07"
	case strings.Contains(head.Schema, "2019-09"):
		return "2019-09"
	case strings.Contains(head.Schema, "draft-06"):
		return "draft-06"
	case strings.Contains(head.Schema, "draft-04"):
		return "draft-04"
	case strings.Contains(head.Schema, "2020-12"):
		return "2020-12"
	}
	return head.Schema
}

// secretUse says which checks use a secret, for its list row.
func secretUse(c emulate.Config, name string) string {
	users := c.SecretUsers(name)
	if len(users) == 0 {
		return "not used"
	}
	return "used by " + strings.Join(users, ", ")
}

func delayLabel(r emulate.Reply) string {
	switch {
	case r.DelayMs == 0 && r.JitterMs == 0:
		return "none"
	case r.JitterMs == 0:
		return duration(r.DelayMs)
	}
	return duration(r.DelayMs) + " + up to " + duration(r.JitterMs) + " at random"
}

func tabCurrent(on bool) string {
	if on {
		return "page"
	}
	return "false"
}

// savedText confirms an edit that went through.
func savedText(section string) string {
	switch {
	case section == "counter":
		return "Counter reset: the next request is number 1."
	case section == "default":
		return "Default reply saved. It applies to the next request."
	case section == "rules", section == "checks", section == "vars":
		return "Deleted. It applies to the next request."
	case section == "secrets", section == "schemas":
		return "Deleted."
	case strings.HasPrefix(section, "schema-"):
		return "Schema saved. Checks that use it apply it to the next request."
	case strings.HasPrefix(section, "secret-"):
		return "Secret saved. Its value is not shown again."
	case strings.HasPrefix(section, "rule-"), strings.HasPrefix(section, "check-"), strings.HasPrefix(section, "var-"):
		kind, n, _ := strings.Cut(section, "-")
		i, err := strconv.Atoi(n)
		if err != nil {
			break
		}
		if kind == "var" {
			kind = "variable"
		}
		return strings.ToUpper(kind[:1]) + kind[1:] + " " + strconv.Itoa(i+1) + " saved. It applies to the next request."
	}
	return "Saved. It applies to the next request."
}

func i64(n int64) string { return strconv.FormatInt(n, 10) }

// blank is n as text, empty for zero (a field not in use).
func blank(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func percent(p float64) string {
	if p == 0 {
		return ""
	}
	return strconv.FormatFloat(p, 'f', -1, 64)
}

// span formats a limit for people: "5 min", "90 s".
func span(d time.Duration) string {
	if d%time.Minute == 0 {
		return strconv.FormatInt(int64(d/time.Minute), 10) + " min"
	}
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + " s"
}

func ms(n int64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatInt(n, 10)
}

func duration(msec int64) string {
	d := time.Duration(msec) * time.Millisecond
	if d < time.Second {
		return strconv.FormatInt(msec, 10) + " ms"
	}
	return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + " s"
}

func ruleAction(r emulate.Rule) string {
	switch r.Do {
	case emulate.ActTimeout:
		return "time out"
	case emulate.ActDrop:
		return "drop the connection"
	}
	if r.Reply.DelayMs > 0 || r.Reply.JitterMs > 0 {
		return strconv.Itoa(r.Reply.Status) + " after " + duration(r.Reply.DelayMs) + "+"
	}
	return strconv.Itoa(r.Reply.Status)
}

// replyOrDefault keeps the reply fields sensible for timeout and drop rules,
// so switching such a rule back to "reply" starts from something valid.
func replyOrDefault(r emulate.Rule) emulate.Reply {
	if r.Do != emulate.ActReply && r.Reply.Status == 0 {
		return emulate.Reply{Status: 500}
	}
	return r.Reply
}

func matchLabel(c emulate.Check) string {
	switch c.Match {
	case emulate.MatchEquals:
		return "equals " + strconv.Quote(c.Value)
	case emulate.MatchRegex:
		return "matches /" + c.Value + "/"
	}
	return "is present"
}

func errorOrStandard(c emulate.Check) emulate.Reply {
	if c.Error != nil {
		return *c.Error
	}
	return emulate.Reply{Status: 400}
}

const templateHelp = `{{.Header "X-Request-Id"}}   first value of a request header
{{.Query "challenge"}}       a query parameter
{{.JSON "event.items.0.id"}} a field of a JSON body (dotted path)
{{.Var.TOKEN}}               an endpoint variable
{{.N}}                       the request number since the counter reset
{{.Method}} {{.Path}} {{.Body}}

Echo a verification challenge:  {"challenge":"{{.JSON "challenge"}}"}`

// answerText says in a sentence what was done with a request.
func answerText(o emulate.Outcome) string {
	var s string
	switch o.Action {
	case emulate.ActTimeout:
		s = "Held without an answer"
		if o.WaitedMs > 0 {
			s += " for " + duration(o.WaitedMs)
		}
		s += ", then closed"
	case emulate.ActAbandoned:
		s = "The sender left after " + duration(o.WaitedMs) + ", before the reply"
		if o.DelayMs > 0 {
			s += "'s " + duration(o.DelayMs) + " delay ran out"
		}
		s += ", so nothing was sent; it would have been " + strconv.Itoa(o.Would)
	case emulate.ActDrop:
		s = "Connection reset without an answer, as a crashed receiver would"
	case emulate.ActError:
		s = "The reply template failed, so 500 was sent"
	default:
		s = strconv.Itoa(o.Status) + " sent"
		if o.DelayMs > 0 {
			s += " after " + duration(o.DelayMs)
		}
	}
	if w := o.Why(); w != "" {
		s += " · " + w
	}
	if o.Counter > 0 {
		s += " · request " + i64(o.Counter) + " since the counter reset"
	}
	return s + "."
}

// selectKey is the discriminator's pointer or header, for the editor.
func selectKey(ch emulate.Check) string {
	if ch.Select == nil {
		return ""
	}
	return ch.Select.Key
}

// formEnctype lets the schema editor upload a file.
func formEnctype(e *editor) string {
	if e.Kind == "schema" {
		return "multipart/form-data"
	}
	return "application/x-www-form-urlencoded"
}

// replyBodyText is what a reply's body field shows: the template or the base64.
func replyBodyText(r emulate.Reply) string {
	if r.BodyBase64 != "" {
		return r.BodyBase64
	}
	return r.Body
}

// rawSize is how many bytes a base64 text holds.
func rawSize(b64 string) int64 {
	return int64(base64.StdEncoding.DecodedLen(len(b64)) - strings.Count(b64, "="))
}
