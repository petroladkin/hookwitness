package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/payload"
	"github.com/petroladkin/hookwitness/internal/picture"
	"github.com/petroladkin/hookwitness/internal/store"
)

// images lists the images a request carries for its exhibit and marks
// their paths in the body.
func (h *Handler) images(req store.Request, vs []auth.Verdict, body *bodyView) []imageView {
	if req.TooLarge {
		return nil
	}
	found := picture.Find(req.Body, contentType(req.Headers), vs)
	paths := map[string]bool{}
	var out []imageView
	for _, f := range found {
		base := h.prefix + "/r/" + strconv.FormatInt(req.ID, 10) + "/image?path=" + url.QueryEscape(f.Path)
		v := imageView{Label: f.Path, Error: f.Error, Download: base + "&download=1"}
		if f.Path == picture.Body {
			v.Label = "the body"
		}
		var note []string
		if f.Type != "" {
			note = append(note, strings.ToUpper(strings.TrimPrefix(f.Type, "image/")))
		}
		if f.Size > 0 {
			note = append(note, size(int64(f.Size)))
		}
		if f.Width > 0 {
			note = append(note, strconv.Itoa(f.Width)+"×"+strconv.Itoa(f.Height)+" px")
		}
		switch f.Source {
		case "schema":
			note = append(note, "declared "+f.Declared)
		case "data URL":
			note = append(note, "a data URL, no schema")
		}
		if f.NotChecked != "" {
			note = append(note, "not checked: "+f.NotChecked)
		}
		v.Note = strings.Join(note, " · ")
		switch {
		case f.Error != "":
		case f.NotChecked != "":
			v.Hint = "not shown: too large to decode; download it"
		case picture.Decodable(f.Type):
			v.Src = base
		default:
			v.Hint = "cannot be shown here (" + f.Type + "); download it"
		}
		if f.Type == "" {
			v.Download = "" // nothing that sniffs as an image to hand out
		}
		out = append(out, v)
		paths[f.Path] = true
	}
	if body.Kind == bodyJSON {
		markImages(body.JSON, paths)
		payload.ElideImages(body.JSON, 48)
	}
	return out
}

// image serves the decoded bytes of an image in a request: inline when
// they sniff as an image a browser shows, else only as a download. The
// type always comes from the bytes, never from the sender.
func (h *Handler) image(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	req, err := h.store.Request(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && req.TooLarge) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, "image", err)
		return
	}
	path := r.URL.Query().Get("path")
	b, err := picture.At(req.Body, path)
	if err != nil {
		http.Error(w, "No image: "+err.Error(), http.StatusNotFound)
		return
	}
	t := picture.Sniff(b)
	download := r.URL.Query().Has("download")
	if t == "" || (!picture.Decodable(t) && !download) {
		http.Error(w, "Not an image that can be shown; add download=1 for its bytes.", http.StatusUnsupportedMediaType)
		return
	}
	name := "request-" + strconv.FormatInt(req.ID, 10) + fileSafe(path) + "." + picture.Ext(t)
	disposition := "inline"
	if download {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", t)
	w.Header().Set("Content-Disposition", disposition+`; filename="`+name+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(b)
}

// fileSafe turns a JSON pointer into a file-name part: "/items/0/image" ->
// "-items-0-image".
func fileSafe(path string) string {
	var sb strings.Builder
	for _, c := range path {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			sb.WriteRune(c)
		default:
			sb.WriteByte('-')
		}
	}
	return strings.TrimRight(sb.String(), "-")
}
