package replay

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/access"
	"github.com/petroladkin/hookwitness/internal/send"
	"github.com/petroladkin/hookwitness/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// caught stores a GitHub-style signed request the way the catcher would.
func caught(t *testing.T, st *store.Store, body []byte) *store.Request {
	t.Helper()
	ctx := context.Background()
	ep, err := st.CreateEndpoint(ctx, "/gh")
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("It's a Secret to Everybody"))
	mac.Write(body)
	r := &store.Request{EndpointID: ep.ID, ReceivedAt: time.Now(), Method: "POST", Path: "/gh", RawQuery: "token=QUERYSECRET",
		Proto: "HTTP/1.1", RemoteAddr: "192.0.2.1:5000", Body: body, Size: int64(len(body)),
		Headers: []store.Header{
			{Name: "Host", Value: "hookwitness.test"},
			{Name: "X-GitHub-Event", Value: "push"},
			{Name: "x-hub-signature-256", Value: "sha256=" + hex.EncodeToString(mac.Sum(nil))},
			{Name: "Content-Type", Value: "application/json"},
			{Name: "Content-Length", Value: strconv.Itoa(len(body))},
		}}
	if err := st.AddRequest(ctx, r, 100, nil); err != nil {
		t.Fatal(err)
	}
	return r
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newService(t *testing.T, st *store.Store, o Options) (*Service, *logBuf) {
	var lb logBuf
	return New(st, slog.New(slog.NewTextHandler(&lb, nil)), o), &lb
}

// The end to end case: a signed request replayed to a handler that checks
// GitHub's signature passes, and the answer is recorded.
func TestReplaySignedRequest(t *testing.T) {
	st := openStore(t)
	body := []byte(`{"ref":"refs/heads/main","n":"\u00e9"}` + "\n")
	r := caught(t, st, body)
	var gotHost, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		mac := hmac.New(sha256.New, []byte("It's a Secret to Everybody"))
		mac.Write(b)
		gotHost, gotPath = req.Host, req.URL.RequestURI()
		if !hmac.Equal([]byte(req.Header.Get("X-Hub-Signature-256")), []byte("sha256="+hex.EncodeToString(mac.Sum(nil)))) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Handler", "ok")
		io.WriteString(w, "verified")
	}))
	defer srv.Close()
	svc, logs := newService(t, st, Options{})
	ctx := access.With(context.Background(), &access.Identity{Name: "ada", Role: "user", Via: access.ViaSession})
	rp, err := svc.Replay(ctx, r.ID, Input{URL: srv.URL + "/webhooks/github?token=QUERYSECRET"})
	if err != nil {
		t.Fatal(err)
	}
	if rp.Status != 200 || string(rp.Body) != "verified" || rp.ErrorKind != "" || rp.By != "ada" {
		t.Fatalf("replay %+v", rp)
	}
	if gotHost != strings.TrimPrefix(srv.URL, "http://") || gotPath != "/webhooks/github?token=QUERYSECRET" {
		t.Fatalf("the handler saw host %q path %q", gotHost, gotPath)
	}
	if rp.Line != "POST /webhooks/github?token=QUERYSECRET HTTP/1.1" || rp.Sent[0].Value != gotHost || rp.Sent[2].Name != "x-hub-signature-256" {
		t.Fatalf("recorded line %q sent %v", rp.Line, rp.Sent)
	}
	if rp.Options != `{"host":"target","insecure_tls":false,"timeout_ms":30000}` {
		t.Fatalf("options %s", rp.Options)
	}
	stored, err := st.Replays(context.Background(), r.ID)
	if err != nil || len(stored) != 1 || stored[0].ID != rp.ID {
		t.Fatalf("stored %v %v", stored, err)
	}
	// The log names the origin, the request, the result and the user; never
	// the query, a header value or the body.
	l := logs.String()
	for _, want := range []string{"request replayed", "target=" + srv.URL, "status=200", "user=ada", "request=" + strconv.FormatInt(r.ID, 10)} {
		if !strings.Contains(l, want) {
			t.Errorf("log lacks %q:\n%s", want, l)
		}
	}
	for _, never := range []string{"QUERYSECRET", "sha256=", "refs/heads", "verified", "X-GitHub-Event", "push"} {
		if strings.Contains(l, never) {
			t.Errorf("log has %q:\n%s", never, l)
		}
	}

	// KeepHost sends the received Host.
	rp, err = svc.Replay(context.Background(), r.ID, Input{URL: srv.URL + "/x", KeepHost: true})
	if err != nil || gotHost != "hookwitness.test" || !strings.Contains(rp.Options, `"host":"received"`) {
		t.Fatalf("keep host: %v, handler saw %q, options %s", err, gotHost, rp.Options)
	}
}

func TestRefusals(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	r := caught(t, st, []byte("{}"))
	svc, logs := newService(t, st, Options{Resolver: stub{"metadata.test": {netip.MustParseAddr("169.254.169.254")}}})
	var inv *InvalidError
	for in, field := range map[string]string{"ftp://x/": "url", "http://u:p@127.0.0.1/": "url", "not a url": "url"} {
		if _, err := svc.Replay(ctx, r.ID, Input{URL: in}); !errors.As(err, &inv) || inv.Field != field {
			t.Errorf("%s: %v", in, err)
		}
	}
	for _, d := range []time.Duration{time.Millisecond, 6 * time.Minute} {
		if _, err := svc.Replay(ctx, r.ID, Input{URL: "http://127.0.0.1:1/", Timeout: d}); !errors.As(err, &inv) || inv.Field != "timeout_ms" {
			t.Errorf("timeout %s: %v", d, err)
		}
	}
	for _, u := range []string{"http://169.254.169.254/latest/meta-data/", "http://metadata.test/", "http://[fe80::1]/"} {
		_, err := svc.Replay(ctx, r.ID, Input{URL: u})
		if !errors.Is(err, send.ErrRefused) {
			t.Errorf("%s: %v", u, err)
		}
	}
	if !strings.Contains(logs.String(), "replay refused by the target policy") || strings.Contains(logs.String(), "meta-data") {
		t.Fatalf("log:\n%s", logs)
	}
	if list, _ := st.Replays(ctx, r.ID); len(list) != 0 {
		t.Fatalf("refusals stored %d replays", len(list))
	}
	if _, err := svc.Replay(ctx, 999, Input{URL: "http://127.0.0.1:1/"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing request: %v", err)
	}
	// Bodies not kept whole.
	big := &store.Request{EndpointID: r.EndpointID, ReceivedAt: time.Now(), Method: "POST", Path: "/gh", Proto: "HTTP/1.1",
		Size: 20 << 20, TooLarge: true}
	st.AddRequest(ctx, big, 100, nil)
	cut := &store.Request{EndpointID: store.Unmatched, ReceivedAt: time.Now(), Method: "POST", Path: "/nope", Proto: "HTTP/1.1",
		Body: bytes.Repeat([]byte("a"), 64<<10), Size: 100 << 10}
	st.AddRequest(ctx, cut, 100, nil)
	var nk *NotKeptError
	if _, err := svc.Replay(ctx, big.ID, Input{URL: "http://127.0.0.1:1/"}); !errors.As(err, &nk) || !strings.Contains(nk.Reason, "not kept") {
		t.Fatalf("too large: %v", err)
	}
	if _, err := svc.Replay(ctx, cut.ID, Input{URL: "http://127.0.0.1:1/"}); !errors.As(err, &nk) || !strings.Contains(nk.Reason, "65536 of 102400") {
		t.Fatalf("cut: %v", err)
	}
	// -replay-allow none.
	off, _ := send.ParsePolicy("none")
	svcOff, _ := newService(t, st, Options{Policy: off})
	if _, err := svcOff.Replay(ctx, r.ID, Input{URL: "http://127.0.0.1:1/"}); !errors.Is(err, send.ErrRefused) || !strings.Contains(err.Error(), "off") {
		t.Fatalf("none: %v", err)
	}
}

type stub map[string][]netip.Addr

func (s stub) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	return s[host], nil
}

// A target that fails is still a replay, with the failure recorded.
func TestTargetFailureIsRecorded(t *testing.T) {
	st := openStore(t)
	r := caught(t, st, []byte("{}"))
	svc, _ := newService(t, st, Options{})
	rp, err := svc.Replay(context.Background(), r.ID, Input{URL: "http://127.0.0.1:1/hook"})
	if err != nil {
		t.Fatal(err)
	}
	if rp.Status != 0 || rp.ErrorKind != "dial" || !strings.Contains(rp.Error, "refused") || rp.ID == 0 {
		t.Fatalf("replay %+v", rp)
	}
}

type budget struct{ free atomic.Int64 }

func (b *budget) Reserve(n int64) bool {
	if b.free.Add(-n) < 0 {
		b.free.Add(n)
		return false
	}
	return true
}
func (b *budget) Release(n int64) { b.free.Add(n) }

// Four at once; a fifth waits for a slot and then is busy; the body's bytes
// count against -max-inflight.
func TestLimits(t *testing.T) {
	st := openStore(t)
	r := caught(t, st, []byte("0123456789"))
	release := make(chan struct{})
	var started atomic.Int32
	blocking := func(ctx context.Context, h send.Head, body []byte, o send.Options) (send.Response, error) {
		started.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return send.Response{Status: 204, Line: h.Method + " " + h.Target + " HTTP/1.1"}, nil
	}
	b := &budget{}
	b.free.Store(1 << 20)
	svc, _ := newService(t, st, Options{Send: blocking, SlotWait: 200 * time.Millisecond, Budget: b})
	var wg sync.WaitGroup
	for range Slots {
		wg.Go(func() {
			if _, err := svc.Replay(context.Background(), r.ID, Input{URL: "http://127.0.0.1:9/"}); err != nil {
				t.Error(err)
			}
		})
	}
	for started.Load() < Slots {
		time.Sleep(5 * time.Millisecond)
	}
	if got := b.free.Load(); got != 1<<20-Slots*10 {
		t.Fatalf("budget held %d bytes, want %d", 1<<20-got, Slots*10)
	}
	start := time.Now()
	if _, err := svc.Replay(context.Background(), r.ID, Input{URL: "http://127.0.0.1:9/"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("fifth: %v", err)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("the fifth did not wait for a slot")
	}
	close(release)
	wg.Wait()
	if b.free.Load() != 1<<20 {
		t.Fatalf("budget not released: %d", b.free.Load())
	}
	// A body that does not fit the budget is busy at once.
	b.free.Store(5)
	if _, err := svc.Replay(context.Background(), r.ID, Input{URL: "http://127.0.0.1:9/"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("over budget: %v", err)
	}
}

// Stop ends replays in flight; they are recorded, and Wait returns.
func TestStop(t *testing.T) {
	st := openStore(t)
	r := caught(t, st, []byte("{}"))
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.ReadAll(req.Body) // then the server notices the client leaving
		<-req.Context().Done()
	}))
	defer hang.Close()
	svc, _ := newService(t, st, Options{})
	done := make(chan store.Replay)
	go func() {
		rp, err := svc.Replay(context.Background(), r.ID, Input{URL: hang.URL})
		if err != nil {
			t.Error(err)
		}
		done <- rp
	}()
	time.Sleep(100 * time.Millisecond)
	svc.Stop()
	select {
	case rp := <-done:
		if rp.ErrorKind != "timeout" || !strings.Contains(rp.Error, "stopped") {
			t.Fatalf("stopped replay %+v", rp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not end the replay")
	}
	svc.Wait()
	if _, err := svc.Replay(context.Background(), r.ID, Input{URL: hang.URL}); !errors.Is(err, ErrStopped) {
		t.Fatalf("after Stop: %v", err)
	}
}
