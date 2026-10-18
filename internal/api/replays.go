package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/petroladkin/hookwitness/internal/replay"
	"github.com/petroladkin/hookwitness/internal/send"
	"github.com/petroladkin/hookwitness/internal/store"
)

// replayJSON is a replay: what was sent where, and what the target
// answered. status 0 and error mean there was no (complete) answer.
type replayJSON struct {
	ID          int64       `json:"id"`
	RequestID   int64       `json:"request_id"`
	CreatedAt   stamp       `json:"created_at"`
	By          string      `json:"by"` // who replayed; "" without login
	URL         string      `json:"url"`
	Host        string      `json:"host"` // target or received
	InsecureTLS bool        `json:"insecure_tls"`
	TimeoutMS   int64       `json:"timeout_ms"`
	Line        string      `json:"line"`
	Sent        [][2]string `json:"sent"`
	Notes       []string    `json:"notes"`
	Proto       string      `json:"proto"`
	Status      int         `json:"status"`
	Reason      string      `json:"reason"`
	Headers     [][2]string `json:"headers"`
	BodySize    int64       `json:"body_size"` // -1 when the answer was cut before its end
	BodyCut     bool        `json:"body_cut"`  // only the first 1 MiB (or what arrived) is kept
	DurationMS  int64       `json:"duration_ms"`
	TTFBMS      int64       `json:"ttfb_ms"`
	Error       *replayErr  `json:"error"`
	BodyText    *string     `json:"body_text,omitempty"`
	BodyBase64  *string     `json:"body_base64,omitempty"`
}

type replayErr struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func pairsOf(hs []store.Header) [][2]string {
	out := make([][2]string, len(hs))
	for i, h := range hs {
		out[i] = [2]string{h.Name, h.Value}
	}
	return out
}

func replayView(rp store.Replay, withBody bool) replayJSON {
	v := replayJSON{ID: rp.ID, RequestID: rp.RequestID, CreatedAt: stamp(rp.CreatedAt), By: rp.By, URL: rp.URL, Line: rp.Line,
		Sent: pairsOf(rp.Sent), Notes: rp.Notes, Proto: rp.Proto, Status: rp.Status, Reason: rp.Reason,
		Headers: pairsOf(rp.Headers), BodySize: rp.Size, BodyCut: rp.Truncated, DurationMS: rp.Duration.Milliseconds(),
		TTFBMS: rp.TTFB.Milliseconds()}
	if v.Notes == nil {
		v.Notes = []string{}
	}
	var o struct {
		Host        string `json:"host"`
		InsecureTLS bool   `json:"insecure_tls"`
		TimeoutMS   int64  `json:"timeout_ms"`
	}
	json.Unmarshal([]byte(rp.Options), &o)
	v.Host, v.InsecureTLS, v.TimeoutMS = o.Host, o.InsecureTLS, o.TimeoutMS
	if rp.ErrorKind != "" {
		v.Error = &replayErr{rp.ErrorKind, rp.Error}
	}
	if withBody && rp.Body != nil {
		if utf8.Valid(rp.Body) {
			s := string(rp.Body)
			v.BodyText = &s
		} else {
			s := base64.StdEncoding.EncodeToString(rp.Body)
			v.BodyBase64 = &s
		}
	}
	return v
}

// replayIn is a replay's request body.
type replayIn struct {
	URL         string `json:"url"`
	Host        string `json:"host"`
	InsecureTLS bool   `json:"insecure_tls"`
	TimeoutMS   *int64 `json:"timeout_ms"`
}

// Input checks a replay's request body; it returns the input or the field
// at fault and why.
func (in replayIn) input() (replay.Input, string, string) {
	out := replay.Input{URL: in.URL, InsecureTLS: in.InsecureTLS}
	if strings.TrimSpace(in.URL) == "" {
		return out, "url", "url is the target to send the request to, such as http://localhost:3000/webhooks"
	}
	switch in.Host {
	case "", "target":
	case "received":
		out.KeepHost = true
	default:
		return out, "host", "host is target (the URL's host and port, the default) or received (the Host line as caught)"
	}
	if in.TimeoutMS != nil {
		if *in.TimeoutMS <= 0 {
			return out, "timeout_ms", "timeout_ms is a number of milliseconds"
		}
		out.Timeout = time.Duration(*in.TimeoutMS) * time.Millisecond
	}
	return out, "", ""
}

// replayRequest sends a caught request to a URL and records the answer.
// The target failing is still a replay (201, error set); a refused target,
// an unkept body or a bad input is not.
func (h *Handler) replayRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		WriteError(w, http.StatusNotFound, "not_found", "no request "+r.PathValue("id"), "")
		return
	}
	var body replayIn
	if !decode(w, r, &body, false) {
		return
	}
	in, field, msg := body.input()
	if field != "" {
		WriteError(w, http.StatusUnprocessableEntity, "invalid", msg, field)
		return
	}
	rp, err := h.replay.Replay(r.Context(), id, in)
	if err != nil {
		if !WriteReplayError(w, err, id) {
			h.fail(w, "replay", err)
		}
		return
	}
	writeJSON(w, http.StatusCreated, replayView(rp, true))
}

// WriteReplayError answers the errors of replay.Service.Replay that have
// an answer, and reports whether it did; anything else is the caller's.
func WriteReplayError(w http.ResponseWriter, err error, id int64) bool {
	var nk *replay.NotKeptError
	var inv *replay.InvalidError
	switch {
	case errors.Is(err, replay.ErrNotFound):
		WriteError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no request %d: it was never caught, or was cleared or pruned", id), "")
	case errors.As(err, &nk):
		WriteError(w, http.StatusConflict, "body_not_kept", nk.Reason, "")
	case errors.As(err, &inv):
		WriteError(w, http.StatusUnprocessableEntity, "invalid", inv.Reason, inv.Field)
	case errors.Is(err, send.ErrRefused):
		WriteError(w, http.StatusUnprocessableEntity, "target_refused", strings.TrimPrefix(err.Error(), send.ErrRefused.Error()+": "), "url")
	case errors.Is(err, replay.ErrBusy):
		w.Header().Set("Retry-After", "5")
		WriteError(w, http.StatusServiceUnavailable, "busy", err.Error(), "")
	case errors.Is(err, replay.ErrStopped):
		WriteError(w, http.StatusServiceUnavailable, "stopping", err.Error(), "")
	default:
		return false
	}
	return true
}

// listReplays lists a request's replays, newest first; include=body adds
// the answers' bodies.
func (h *Handler) listReplays(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		WriteError(w, http.StatusNotFound, "not_found", "no request "+r.PathValue("id"), "")
		return
	}
	withBody := false
	for _, s := range r.URL.Query()["include"] {
		if s != "body" {
			WriteError(w, http.StatusUnprocessableEntity, "invalid", "include is body", "include")
			return
		}
		withBody = true
	}
	if _, err := h.store.Request(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no request %d: it was never caught, or was cleared or pruned", id), "")
		return
	} else if err != nil {
		h.fail(w, "replays", err)
		return
	}
	list, err := h.store.Replays(r.Context(), id)
	if err != nil {
		h.fail(w, "replays", err)
		return
	}
	out := make([]replayJSON, 0, len(list))
	for _, rp := range list {
		if withBody {
			if rp, err = h.store.Replay(r.Context(), rp.ID); err != nil {
				continue // replaced by a newer one meanwhile
			}
		}
		out = append(out, replayView(rp, withBody))
	}
	writeJSON(w, http.StatusOK, map[string]any{"replays": out})
}

func (h *Handler) loadReplay(w http.ResponseWriter, r *http.Request) (store.Replay, bool) {
	id, ok := pathID(r, "id")
	if !ok {
		WriteError(w, http.StatusNotFound, "not_found", "no replay "+r.PathValue("id"), "")
		return store.Replay{}, false
	}
	rp, err := h.store.Replay(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		WriteError(w, http.StatusNotFound, "not_found", fmt.Sprintf("no replay %d: it went with its request, or newer replays replaced it", id), "")
		return store.Replay{}, false
	}
	if err != nil {
		h.fail(w, "replay", err)
		return store.Replay{}, false
	}
	return rp, true
}

func (h *Handler) getReplay(w http.ResponseWriter, r *http.Request) {
	if rp, ok := h.loadReplay(w, r); ok {
		writeJSON(w, http.StatusOK, replayView(rp, true))
	}
}

// replayBody returns the target's answer body as it arrived (never
// decompressed), with its Content-Type, always as a download in a sandbox.
func (h *Handler) replayBody(w http.ResponseWriter, r *http.Request) {
	rp, ok := h.loadReplay(w, r)
	if !ok {
		return
	}
	ct := "application/octet-stream"
	for _, hd := range rp.Headers {
		if strings.EqualFold(hd.Name, "Content-Type") {
			if _, _, err := mime.ParseMediaType(hd.Value); err == nil {
				ct = hd.Value
			}
			break
		}
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="replay-%d.body"`, rp.ID))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Content-Length", strconv.Itoa(len(rp.Body)))
	w.Write(rp.Body)
}
