package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/petroladkin/hookwitness/internal/replay"
	"github.com/petroladkin/hookwitness/internal/send"
	"github.com/petroladkin/hookwitness/internal/store"
)

// replayDialogView is the dialog that sends a request again.
type replayDialogView struct {
	Action   string // where the form posts
	Close    string // the exhibit
	URL      string
	KeepHost bool
	Insecure bool
	Lines    int    // header lines of the request
	Chunked  bool   // it came chunked
	Proto    string // as received
	Policy   string // the target policy, when it narrows anything
	Timeout  time.Duration
	Error    string
	Field    string // url, or "" for an error of the request
	Blocked  string // why it cannot be replayed at all
}

// replayView is one replay under its request.
type replayView struct {
	ID        int64
	At        time.Time
	By        string
	URL       string
	Host      string
	Insecure  bool
	TimeoutMS int64
	Line      string
	Sent      []store.Header
	Notes     []string
	Status    int
	Reason    string
	Headers   []store.Header
	Body      bodyView
	Size      int64
	Cut       bool
	Duration  time.Duration
	TTFB      time.Duration
	ErrorKind string
	Error     string
	Download  string
}

// good reports whether the target answered 2xx.
func (v replayView) good() bool { return v.ErrorKind == "" && v.Status >= 200 && v.Status < 300 }

// stamp is the replay's result in a word or two: "401 Unauthorized", "no answer: timeout".
func (v replayView) stamp() string {
	if v.Status == 0 {
		return "no answer: " + v.ErrorKind
	}
	s := strconv.Itoa(v.Status)
	if v.Reason != "" {
		s += " " + v.Reason
	}
	return s
}

func (h *Handler) replays(ctx context.Context, requestID int64) ([]replayView, error) {
	list, err := h.store.Replays(ctx, requestID)
	if err != nil {
		return nil, err
	}
	out := make([]replayView, 0, len(list))
	for _, rp := range list {
		full, err := h.store.Replay(ctx, rp.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var o struct {
			Host        string `json:"host"`
			InsecureTLS bool   `json:"insecure_tls"`
			TimeoutMS   int64  `json:"timeout_ms"`
		}
		json.Unmarshal([]byte(full.Options), &o)
		out = append(out, replayView{ID: full.ID, At: full.CreatedAt, By: full.By, URL: full.URL, Host: o.Host,
			Insecure: o.InsecureTLS, TimeoutMS: o.TimeoutMS, Line: full.Line, Sent: full.Sent, Notes: full.Notes,
			Status: full.Status, Reason: full.Reason, Headers: full.Headers, Body: viewBody(full.Body), Size: full.Size,
			Cut: full.Truncated, Duration: full.Duration, TTFB: full.TTFB, ErrorKind: full.ErrorKind, Error: full.Error,
			Download: h.prefix + "/replays/" + strconv.FormatInt(full.ID, 10) + "/body"})
	}
	return out, nil
}

// replayDefaults opens the dialog on an exhibit: the last target used for
// this register, else the request's own path on localhost.
func (h *Handler) replayDefaults(ctx context.Context, req store.Request, ex *exhibitView) (*replayDialogView, error) {
	v := &replayDialogView{Action: h.prefix + "/r/" + strconv.FormatInt(req.ID, 10) + "/replay", Close: ex.Self,
		Lines: len(req.Headers), Proto: req.Proto, Timeout: h.replay.DefaultTimeout()}
	for _, hd := range req.Headers {
		if strings.EqualFold(hd.Name, "Transfer-Encoding") && strings.Contains(strings.ToLower(hd.Value), "chunked") {
			v.Chunked = true
		}
	}
	if p := h.replay.Policy(); p.Off() || p.String() != (send.Policy{}).String() {
		v.Policy = p.String()
	}
	switch {
	case h.replay.Policy().Off():
		v.Blocked = "Replay is off on this server (-replay-allow none)."
	case req.TooLarge:
		v.Blocked = "The body of this request was not kept, so it cannot be sent as it arrived."
	case int64(len(req.Body)) < req.Size:
		v.Blocked = "Only the start of this body was kept (a path with no endpoint), so it cannot be sent as it arrived."
	}
	last, err := h.store.LastReplayURL(ctx, req.EndpointID)
	if err != nil {
		return nil, err
	}
	v.URL = last
	if v.URL == "" {
		v.URL = "http://localhost:3000" + req.Path
		if req.RawQuery != "" {
			v.URL += "?" + req.RawQuery
		}
	}
	return v, nil
}

// replayRequest sends a request again from the dialog; the exhibit then
// shows the answer at the top of its replays. A refusal opens the dialog
// again with the reason.
func (h *Handler) replayRequest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	in := replay.Input{URL: strings.TrimSpace(r.PostFormValue("url")), KeepHost: r.PostFormValue("host") == "received",
		InsecureTLS: r.PostFormValue("insecure_tls") == "true"}
	rp, err := h.replay.Replay(ctx, id, in)
	if err == nil {
		req, lerr := h.store.Request(ctx, id)
		if lerr != nil {
			http.Redirect(w, r, h.prefix+"/", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, h.regPath(req.EndpointID)+"/r/"+strconv.FormatInt(id, 10)+"#replay-"+strconv.FormatInt(rp.ID, 10), http.StatusSeeOther)
		return
	}
	if errors.Is(err, replay.ErrNotFound) {
		h.notFound(w, r, "This request is no longer kept: it was cleared, pruned or deleted.")
		return
	}
	msg, field, status := "", "", http.StatusUnprocessableEntity
	var nk *replay.NotKeptError
	var inv *replay.InvalidError
	switch {
	case errors.As(err, &inv):
		msg, field = inv.Reason, inv.Field
	case errors.Is(err, send.ErrRefused):
		msg, field = strings.TrimPrefix(err.Error(), send.ErrRefused.Error()+": "), "url"
	case errors.As(err, &nk):
		msg, status = nk.Reason, http.StatusConflict
	case errors.Is(err, replay.ErrBusy), errors.Is(err, replay.ErrStopped):
		msg, status = err.Error(), http.StatusServiceUnavailable
	default:
		h.fail(w, "replay", err)
		return
	}
	req, err := h.store.Request(ctx, id)
	if err != nil {
		h.notFound(w, r, "This request is no longer kept: it was cleared, pruned or deleted.")
		return
	}
	ep, err := h.store.Endpoint(ctx, req.EndpointID)
	if err != nil {
		h.fail(w, "replay", err)
		return
	}
	ex, err := h.exhibit(ctx, req)
	if err != nil {
		h.fail(w, "replay", err)
		return
	}
	d, err := h.replayDefaults(ctx, req, &ex)
	if err != nil {
		h.fail(w, "replay", err)
		return
	}
	d.URL, d.KeepHost, d.Insecure, d.Error, d.Field = in.URL, in.KeepHost, in.InsecureTLS, msg, field
	h.showRegister(w, r, req.EndpointID, ep, &ex, nil, nil, d, status)
}

// replayBody is a replay's answer body, always as a download.
func (h *Handler) replayBody(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rp, err := h.store.Replay(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, "replay body", err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="replay-`+strconv.FormatInt(rp.ID, 10)+`.body"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(rp.Body)))
	w.Header().Set("Cache-Control", "no-store")
	w.Write(rp.Body)
}

func millisOf(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) + " ms" }
