package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/petroladkin/hookwitness/internal/api"
	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/setup"
	"github.com/petroladkin/hookwitness/internal/store"
)

// importView is the import dialog: pick a file (or paste it), then review
// what it creates or replaces and give secret values if wanted.
type importView struct {
	Review bool   // the second step
	Text   string // the file, carried from the first step to the second
	Error  string
	Items  []importItem
}

type importItem struct {
	I       int
	Path    string // in the file
	Target  string // where it goes: the file's path or another one typed
	Taken   bool   // an endpoint is already at Target
	Replace bool
	Summary string // what the endpoint does, in a few words
	Secrets []setup.Secret
}

func (h *Handler) setupOptions() setup.Options {
	return setup.Options{Reserved: h.reserve, MaxWait: h.maxWait}
}

// exportSetup downloads the setup file of one endpoint, or of all of them
// (the route without a register).
func (h *Handler) exportSetup(w http.ResponseWriter, r *http.Request) {
	var eps []store.Endpoint
	if r.PathValue("reg") == "" {
		var err error
		if eps, err = h.store.Endpoints(r.Context()); err != nil {
			h.fail(w, "export setup", err)
			return
		}
	} else {
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
			h.fail(w, "export setup", err)
			return
		}
		eps = []store.Endpoint{ep}
	}
	f, name, err := api.ExportSetup(eps)
	if err != nil {
		h.fail(w, "export setup", err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(f.JSON())
}

// maxImportForm bounds an import form's body.
const maxImportForm = 3*setup.MaxSize + 1<<20

// importForm reads the file of the first step: an uploaded file wins over
// pasted text.
func importForm(r *http.Request) (string, error) {
	if err := r.ParseMultipartForm(1 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return "", fmt.Errorf("the file is over %d MiB", setup.MaxSize>>20)
		}
		return "", fmt.Errorf("the form could not be read: %v", err)
	}
	text := r.PostFormValue("text")
	if f, hdr, err := r.FormFile("file"); err == nil {
		defer f.Close()
		if hdr.Size > setup.MaxSize {
			return text, fmt.Errorf("the file is over %d MiB", setup.MaxSize>>20)
		}
		b, err := io.ReadAll(io.LimitReader(f, setup.MaxSize+1))
		if err != nil {
			return text, fmt.Errorf("the file could not be read: %v", err)
		}
		if len(b) > 0 {
			text = string(b)
		}
	}
	if strings.TrimSpace(text) == "" {
		return text, errors.New("choose a setup file or paste its content")
	}
	if len(text) > setup.MaxSize {
		return text, fmt.Errorf("the setup is over %d MiB", setup.MaxSize>>20)
	}
	return text, nil
}

// importSetup is both steps of the import dialog: op "review" reads the
// file and shows what it does; op "import" applies it with the paths,
// replacements and secret values chosen.
func (h *Handler) importSetup(w http.ResponseWriter, r *http.Request) {
	show := func(status int, v importView) { h.showEndpointsWith(w, r, status, endpointsDialog{Import: &v}) }
	// The form is bounded before it is read: a setup file, sent once more
	// (escaped) with the second step, and the fields beside it.
	r.Body = http.MaxBytesReader(w, r.Body, maxImportForm)
	text, err := importForm(r)
	if err != nil {
		show(http.StatusUnprocessableEntity, importView{Text: text, Error: err.Error()})
		return
	}
	f, err := setup.Parse([]byte(text), h.setupOptions())
	if err != nil {
		show(http.StatusUnprocessableEntity, importView{Text: text, Error: err.Error()})
		return
	}
	v := importView{Review: true, Text: text}
	for i, e := range f.Endpoints {
		it := importItem{I: i, Path: e.Path, Target: e.Path, Summary: setupSummary(e), Secrets: e.Secrets}
		if r.PostFormValue("op") == "import" {
			it.Target = strings.TrimSpace(r.PostFormValue("path_" + strconv.Itoa(i)))
			it.Replace = r.PostFormValue("replace_"+strconv.Itoa(i)) != ""
		}
		v.Items = append(v.Items, it)
	}
	if err := h.markTaken(r, v.Items); err != nil {
		h.fail(w, "import setup", err)
		return
	}
	if r.PostFormValue("op") != "import" {
		show(http.StatusOK, v)
		return
	}
	// The paths chosen, checked as the file's own would be.
	replace := map[string]bool{}
	for i, it := range v.Items {
		f.Endpoints[i].Path = it.Target
		if it.Taken && !it.Replace {
			v.Error = it.Target + " is already an endpoint: tick Replace to give it this behaviour (its requests stay), or import under another path."
			show(http.StatusUnprocessableEntity, v)
			return
		}
		replace[it.Target] = it.Replace
	}
	if f, err = setup.Parse(f.JSON(), h.setupOptions()); err != nil {
		v.Error = err.Error()
		show(http.StatusUnprocessableEntity, v)
		return
	}
	// Values typed for secrets, sealed before anything is written.
	values := map[string]emulate.Secret{}
	for i, it := range v.Items {
		for _, s := range it.Secrets {
			val := r.PostFormValue("secret_" + strconv.Itoa(i) + "_" + s.Name)
			if val == "" {
				continue
			}
			sealed, err := emulate.SealSecret(s.Name, s.Kind, val, h.keysOrNil())
			if err != nil {
				v.Error = fmt.Sprintf("%s of %s: %v", s.Name, it.Target, err)
				show(http.StatusUnprocessableEntity, v)
				return
			}
			values[it.Target+"\x00"+s.Name] = sealed
		}
	}
	plan := setup.Plan{
		Replace: func(p string) bool { return replace[p] },
		Value: func(p, name, kind string) (emulate.Secret, bool) {
			s, ok := values[p+"\x00"+name]
			return s, ok && s.Kind == kind
		},
	}
	res, status, err := h.api.ApplyFile(r, f, plan)
	switch {
	case status == http.StatusConflict:
		// Someone took a path meanwhile.
		h.markTaken(r, v.Items)
		v.Error = "a path became an endpoint meanwhile; check the paths again"
		show(status, v)
		return
	case err != nil && status == http.StatusInternalServerError:
		h.fail(w, "import setup", err)
		return
	case err != nil:
		v.Error = err.Error()
		show(status, v)
		return
	}
	q := url.Values{}
	unset := 0
	for _, x := range res {
		q.Add("imported", x.Result+" "+x.Path)
		unset += len(x.Unset)
	}
	if unset > 0 {
		q.Set("unset", strconv.Itoa(unset))
	}
	http.Redirect(w, r, h.prefix+"/?"+q.Encode(), http.StatusSeeOther)
}

// keysOrNil keeps a nil *secret.Key a nil interface.
func (h *Handler) keysOrNil() emulate.Sealer {
	if h.keys == nil {
		return nil
	}
	return h.keys
}

func (h *Handler) markTaken(r *http.Request, items []importItem) error {
	for i := range items {
		_, _, _, err := h.store.EndpointByPath(r.Context(), items[i].Target)
		switch {
		case errors.Is(err, store.ErrNotFound):
			items[i].Taken = false
		case err != nil:
			return err
		default:
			items[i].Taken = true
		}
	}
	return nil
}

// setupSummary says in a few words what an endpoint of a file does.
func setupSummary(e setup.Endpoint) string {
	parts := []string{"default " + strconv.Itoa(e.Config(nil).Default.Status)}
	for _, x := range []struct {
		n    int
		what string
	}{{len(e.Rules), "rule"}, {len(e.Checks), "check"}, {len(e.Schemas), "schema"}, {len(e.Vars), "variable"}} {
		if x.n == 1 {
			parts = append(parts, "1 "+x.what)
		} else if x.n > 1 {
			parts = append(parts, strconv.Itoa(x.n)+" "+x.what+"s")
		}
	}
	return strings.Join(parts, " · ")
}

// unsetSecrets counts an endpoint's secrets without a value.
func unsetSecrets(config string) int {
	c, err := emulate.Parse(config)
	if err != nil {
		return 0
	}
	return len(setup.Unset(c))
}

// plural is "1 secret" or "3 secrets" (one and many name the noun with
// what follows it).
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// pick is a when cond, else b.
func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
