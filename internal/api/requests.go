package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/picture"
	"github.com/petroladkin/hookwitness/internal/store"
)

// Limits of a request list.
const (
	defaultLimit = 50
	maxLimit     = 500
	maxWaitCall  = 60 * time.Second
)

// summaryJSON is a caught request in a list.
type summaryJSON struct {
	ID          int64     `json:"id"`
	EndpointID  *int64    `json:"endpoint_id"` // null for the Unmatched register
	No          int64     `json:"no"`          // number within its register
	ReceivedAt  time.Time `json:"received_at"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Query       string    `json:"query"`
	Status      int       `json:"status"`  // what hookwitness answered; 0 while nothing was sent
	Verdict     string    `json:"verdict"` // pass, fail, n/v; "" without authorization checks
	BodySize    int64     `json:"body_size"`
	BodyKept    bool      `json:"body_kept"` // false when the body was over the limit (413)
	ContentType string    `json:"content_type"`
}

// requestJSON is one caught request in full.
type requestJSON struct {
	summaryJSON
	Proto      string          `json:"proto"`
	RemoteAddr string          `json:"remote_addr"`
	Headers    [][2]string     `json:"headers"` // as received: order, case and repeats kept
	BodyText   *string         `json:"body_text,omitempty"`
	BodyBase64 *string         `json:"body_base64,omitempty"`
	Outcome    json.RawMessage `json:"outcome"`  // what was done with it
	Verdicts   json.RawMessage `json:"verdicts"` // one per authorization check
	Images     []imageJSON     `json:"images"`   // pictures in the body; bytes at /requests/{id}/image?path=
}

// imageJSON is an image a request carries: declared by a schema check, a
// data:image URL in a JSON body, or the body itself (path "").
type imageJSON struct {
	auth.Image
	Source string `json:"source"` // schema, data URL, body
	URL    string `json:"url"`    // the decoded bytes, when they are an image
}

func summaryView(req store.Request) summaryJSON {
	v := summaryJSON{ID: req.ID, No: req.No, ReceivedAt: req.ReceivedAt, Method: req.Method, Path: req.Path,
		Query: req.RawQuery, Status: req.Status, Verdict: req.Verdict, BodySize: req.Size, BodyKept: !req.TooLarge}
	if req.EndpointID != store.Unmatched {
		id := req.EndpointID
		v.EndpointID = &id
	}
	for _, hd := range req.Headers {
		if strings.EqualFold(hd.Name, "Content-Type") {
			v.ContentType = hd.Value
			break
		}
	}
	return v
}

func requestView(req store.Request, base string) requestJSON {
	v := requestJSON{summaryJSON: summaryView(req), Proto: req.Proto, RemoteAddr: req.RemoteAddr,
		Headers: make([][2]string, len(req.Headers)), Outcome: json.RawMessage("null"), Verdicts: json.RawMessage("[]")}
	for i, hd := range req.Headers {
		v.Headers[i] = [2]string{hd.Name, hd.Value}
	}
	if !req.TooLarge {
		if utf8.Valid(req.Body) {
			s := string(req.Body)
			v.BodyText = &s
		} else {
			s := base64.StdEncoding.EncodeToString(req.Body)
			v.BodyBase64 = &s
		}
	}
	if json.Valid([]byte(req.Outcome)) {
		v.Outcome = json.RawMessage(req.Outcome)
	}
	if json.Valid([]byte(req.Verdicts)) {
		v.Verdicts = json.RawMessage(req.Verdicts)
	}
	v.Images = []imageJSON{}
	if !req.TooLarge {
		var vs []auth.Verdict
		json.Unmarshal([]byte(req.Verdicts), &vs)
		for _, f := range picture.Find(req.Body, v.ContentType, vs) {
			img := imageJSON{Image: f.Image, Source: f.Source}
			if f.Type != "" {
				img.URL = base + "/requests/" + strconv.FormatInt(req.ID, 10) + "/image?path=" + url.QueryEscape(f.Path)
			}
			v.Images = append(v.Images, img)
		}
	}
	return v
}

// getImage returns the decoded bytes of an image in a request, typed by
// what they are (never by what the sender claimed).
func (h *Handler) getImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		WriteError(w, http.StatusNotFound, "not_found", "no request "+r.PathValue("id"), "")
		return
	}
	req, err := h.store.Request(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no request %d", id), "")
		return
	}
	if err != nil {
		h.fail(w, "image", err)
		return
	}
	if req.TooLarge {
		WriteError(w, http.StatusNotFound, "not_found", "the body of this request was not kept (over the limit)", "")
		return
	}
	b, err := picture.At(req.Body, r.URL.Query().Get("path"))
	if err != nil {
		WriteError(w, http.StatusNotFound, "not_found", err.Error(), "path")
		return
	}
	t := picture.Sniff(b)
	if t == "" {
		WriteError(w, http.StatusUnprocessableEntity, "not_an_image", "the value is not a PNG, JPEG, GIF, WebP, HEIC or AVIF image", "path")
		return
	}
	w.Header().Set("Content-Type", t)
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(b)
}

func (h *Handler) getRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		WriteError(w, http.StatusNotFound, "not_found", "no request "+r.PathValue("id"), "")
		return
	}
	req, err := h.store.Request(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no request %d: it was never caught, or was cleared or pruned", id), "")
		return
	}
	if err != nil {
		h.fail(w, "request", err)
		return
	}
	writeJSON(w, http.StatusOK, requestView(req, h.base))
}

// listQuery is a request list's parameters.
type listQuery struct {
	after, before int64
	hasAfter      bool
	limit         int
	wait          time.Duration
}

func parseList(w http.ResponseWriter, r *http.Request) (listQuery, bool) {
	q := r.URL.Query()
	l := listQuery{limit: defaultLimit}
	bad := func(field, msg string) (listQuery, bool) {
		WriteError(w, http.StatusUnprocessableEntity, "invalid", msg, field)
		return listQuery{}, false
	}
	id := func(name string) (int64, bool) {
		n, err := strconv.ParseInt(q.Get(name), 10, 64)
		return n, err == nil && n >= 0
	}
	var ok bool
	if q.Has("after") {
		if l.after, ok = id("after"); !ok {
			return bad("after", "after is a request id (0 for the start)")
		}
		l.hasAfter = true
	}
	if q.Has("before") {
		if l.before, ok = id("before"); !ok {
			return bad("before", "before is a request id")
		}
		if l.hasAfter {
			return bad("before", "use after or before, not both")
		}
	}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 {
			return bad("limit", fmt.Sprintf("limit is a number from 1 to %d", maxLimit))
		}
		l.limit = min(n, maxLimit)
	}
	if q.Has("wait") {
		s := q.Get("wait")
		d, err := time.ParseDuration(s)
		if n, nerr := strconv.Atoi(s); nerr == nil {
			d, err = time.Duration(n)*time.Second, nil
		}
		if err != nil || d < 0 || d > maxWaitCall {
			return bad("wait", "wait is a duration up to 60s, such as 30s or 30")
		}
		if !l.hasAfter {
			return bad("wait", "wait needs after: the id of the last request you have (0 for none)")
		}
		l.wait = d
	}
	return l, true
}

// listRequests lists an endpoint's requests: newest first (paged back with
// before), or with after the newer ones oldest first, waiting up to wait
// for the first of them.
func (h *Handler) listRequests(w http.ResponseWriter, r *http.Request) {
	ep, ok := h.endpoint(w, r)
	if !ok {
		return
	}
	l, ok := parseList(w, r)
	if !ok {
		return
	}
	var reqs []store.Request
	var err error
	if l.hasAfter {
		reqs, err = h.waitAfter(r, ep.ID, l)
	} else {
		reqs, err = h.store.Requests(r.Context(), ep.ID, l.before, l.limit)
	}
	if r.Context().Err() != nil {
		return // the client went away
	}
	if err != nil {
		h.fail(w, "list requests", err)
		return
	}
	out := make([]summaryJSON, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, summaryView(req))
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": out})
}

// waitAfter returns the requests after l.after, oldest first; when there
// are none it waits for a new one until l.wait passes, the client leaves or
// the server stops. The broker only wakes it: what is new is read from the
// store, so nothing is missed between the read and the wait.
func (h *Handler) waitAfter(r *http.Request, id int64, l listQuery) ([]store.Request, error) {
	wake, unsubscribe := h.live.Subscribe(id)
	defer unsubscribe()
	deadline := time.NewTimer(l.wait)
	defer deadline.Stop()
	for {
		reqs, err := h.store.RequestsAfter(r.Context(), id, l.after)
		if err != nil || len(reqs) > 0 || l.wait == 0 {
			if len(reqs) > l.limit {
				reqs = reqs[:l.limit]
			}
			return reqs, err
		}
		select {
		case <-wake:
		case <-deadline.C:
			return nil, nil
		case <-h.o.Stop:
			return nil, nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
}
