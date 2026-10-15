package report

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden reports in testdata/report")

type fixtureRequest struct {
	ID         int64           `json:"id"`
	EndpointID int64           `json:"endpoint_id"`
	No         int64           `json:"no"`
	ReceivedAt time.Time       `json:"received_at"`
	Method     string          `json:"method"`
	Path       string          `json:"path"`
	RawQuery   string          `json:"raw_query"`
	Proto      string          `json:"proto"`
	RemoteAddr string          `json:"remote_addr"`
	Headers    [][2]string     `json:"headers"`
	Body       string          `json:"body"`
	BodyB64    string          `json:"body_b64"`
	BodySHA256 string          `json:"body_sha256"`
	Size       int64           `json:"size"`
	TooLarge   bool            `json:"too_large"`
	Status     int             `json:"status"`
	Outcome    json.RawMessage `json:"outcome"`
	Verdict    string          `json:"verdict"`
	Verdicts   json.RawMessage `json:"verdicts"`
}

func fixture(t testing.TB) []store.Request {
	t.Helper()
	b, err := os.ReadFile("../../testdata/report/requests.json")
	if err != nil {
		t.Fatal(err)
	}
	var fs []fixtureRequest
	if err := json.Unmarshal(b, &fs); err != nil {
		t.Fatal(err)
	}
	var out []store.Request
	for _, f := range fs {
		r := store.Request{
			ID: f.ID, EndpointID: f.EndpointID, No: f.No, ReceivedAt: f.ReceivedAt, Method: f.Method, Path: f.Path,
			RawQuery: f.RawQuery, Proto: f.Proto, RemoteAddr: f.RemoteAddr, Size: f.Size, TooLarge: f.TooLarge,
			Status: f.Status, Outcome: string(f.Outcome), Verdict: f.Verdict,
		}
		if string(f.Verdicts) != "null" {
			r.Verdicts = string(f.Verdicts)
		}
		for _, h := range f.Headers {
			r.Headers = append(r.Headers, store.Header{Name: h[0], Value: h[1]})
		}
		r.Body = []byte(f.Body)
		if f.BodyB64 != "" {
			r.Body, _ = base64.StdEncoding.DecodeString(f.BodyB64)
		}
		r.BodySHA256, _ = hex.DecodeString(f.BodySHA256)
		out = append(out, r)
	}
	return out
}

// sliceSource yields the requests, without bodies when asked.
func sliceSource(rs []store.Request) Source {
	return func(ctx context.Context, withBody bool, through int64, fn func(store.Request) error) error {
		for _, r := range rs {
			if through > 0 && r.ID > through {
				continue
			}
			if !withBody {
				r.Body = nil
			}
			if err := fn(r); err != nil {
				return err
			}
		}
		return nil
	}
}

func goldenOptions() Options {
	return Options{
		Endpoint:  "/orders",
		Selection: "whole register",
		Mask:      []string{"Authorization", "cookie", "X-Hub-Signature-256"},
		Version:   "v0.0.0-test",
		Generated: time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC),
		BodyLimit: 256,
	}
}

func render(t testing.TB, f Format, o Options, rs []store.Request) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, f, o, sliceSource(rs)); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestGolden(t *testing.T) {
	rs := fixture(t)
	for _, f := range []Format{Markdown, HTML} {
		t.Run(string(f), func(t *testing.T) {
			got := render(t, f, goldenOptions(), rs)
			path := "../../testdata/report/report." + string(f)
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("%s differs from %s; run with -update after an intended change and review the diff", f, path)
			}
		})
	}
}

// TestOnlyTheGenerationTimeDiffers: the same requests and options give the
// same bytes; another generation time changes only the lines that name it.
func TestOnlyTheGenerationTimeDiffers(t *testing.T) {
	rs := fixture(t)
	for _, f := range []Format{Markdown, HTML} {
		a := render(t, f, goldenOptions(), rs)
		if b := render(t, f, goldenOptions(), rs); a != b {
			t.Fatalf("%s: two runs differ", f)
		}
		o := goldenOptions()
		o.Generated = o.Generated.Add(time.Hour)
		al, bl := strings.Split(a, "\n"), strings.Split(render(t, f, o, rs), "\n")
		if len(al) != len(bl) {
			t.Fatalf("%s: %d lines vs %d", f, len(al), len(bl))
		}
		for i := range al {
			if al[i] != bl[i] && !strings.Contains(al[i], "2026-10-12T09:30:00.000Z") {
				t.Fatalf("%s line %d differs:\n%s\n%s", f, i+1, al[i], bl[i])
			}
		}
	}
}

func TestMasking(t *testing.T) {
	rs := fixture(t)
	secrets := []string{"test-token-0123456789abcdef", "session=abc", "sha256=5d1f8a0c2b9e", "received <code>sha256=00</code>"}
	for _, f := range []Format{Markdown, HTML} {
		masked := render(t, f, goldenOptions(), rs)
		for _, s := range secrets {
			if strings.Contains(masked, s) {
				t.Fatalf("%s: %q is in a masked report", f, s)
			}
		}
		for _, want := range []string{"•••• 34 chars", "•••• 23 chars", "•••• 71 chars"} {
			if !strings.Contains(masked, want) {
				t.Fatalf("%s: no %q", f, want)
			}
		}
		o := goldenOptions()
		o.Mask = nil
		plain := render(t, f, o, rs)
		for _, s := range secrets[:3] {
			if !strings.Contains(plain, s) {
				t.Fatalf("%s: %q missing without masking", f, s)
			}
		}
		if strings.Contains(plain, "••••") {
			t.Fatalf("%s: masked without masking", f)
		}
	}
	if md := render(t, Markdown, goldenOptions(), rs); !strings.Contains(md, "| Masked headers | `Authorization`, `cookie`, `X-Hub-Signature-256` |") {
		t.Fatal("the header does not name the masked headers")
	}
}

func TestSummary(t *testing.T) {
	h, err := summarize(context.Background(), goldenOptions(), sliceSource(fixture(t)))
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, c := range h.Outcomes {
		counts[c.Label] = c.N
	}
	want := map[string]int{"2xx": 3, "4xx": 1, "5xx": 1, "timeout": 1, "drop": 1, "refused by a check": 1}
	for k, n := range want {
		if counts[k] != n {
			t.Errorf("%s: %d, want %d (%+v)", k, counts[k], n, h.Outcomes)
		}
	}
	if len(h.Checks) != 2 || h.Checks[1] != (CheckCount{Check: 2, Label: "JSON Schema by /type", Pass: 3, Fail: 1, NC: 1, NV: 1}) {
		t.Errorf("checks %+v", h.Checks)
	}
	if len(h.Retries) != 1 || len(h.Retries[0].Nos) != 2 || h.Retries[0].Gaps[0] != 1500*time.Millisecond {
		t.Errorf("retries %+v", h.Retries)
	}
	if !h.From.Equal(time.Date(2026, 10, 11, 12, 0, 1, 250e6, time.UTC)) || !h.To.Equal(time.Date(2026, 10, 11, 12, 1, 5, 500e6, time.UTC)) {
		t.Errorf("range %v – %v", h.From, h.To)
	}
}

func TestFileName(t *testing.T) {
	at := time.Date(2026, 10, 12, 9, 30, 5, 0, time.FixedZone("x", 3600))
	for endpoint, want := range map[string]string{
		"/orders":             "hookwitness-orders-20261012T083005Z.md",
		"/stripe/Webhooks/v2": "hookwitness-stripe-webhooks-v2-20261012T083005Z.md",
		"/":                   "hookwitness-root-20261012T083005Z.md",
		"":                    "hookwitness-unmatched-20261012T083005Z.md",
		"/замовлення":         "hookwitness-root-20261012T083005Z.md",
	} {
		if got := FileName(endpoint, at, Markdown); got != want {
			t.Errorf("%q: %s, want %s", endpoint, got, want)
		}
	}
}

func TestMarkdownFences(t *testing.T) {
	md := render(t, Markdown, goldenOptions(), fixture(t))
	// The text body has a run of four backticks: its fence has five.
	if !strings.Contains(md, "\n`````\nline one\n```\nnot a fence end\n````\nline | with | pipes\n`````\n") {
		t.Fatal("a body with backticks is not fenced intact")
	}
	if !strings.Contains(md, "| `a` | `|` |") && !strings.Contains(md, "| `a` | `\\|` |") {
		t.Fatal("a pipe in a query value breaks the table")
	}
}

func TestReportImages(t *testing.T) {
	rs := fixture(t)
	md := render(t, Markdown, goldenOptions(), rs)
	for _, want := range []string{
		"| `/avatar` | PNG · 91 B · ",
		"24×16 px · declared image/png |",
		"| `/badge` | GIF · ",
		"8×8 px · a data URL, no schema |",
		"| `the body` | PNG · 70 B · 1×1 px · not valid: a truncated or corrupt PNG",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown lacks %q", want)
		}
	}
	html := render(t, HTML, goldenOptions(), rs)
	if strings.Count(html, `<img src="data:image/`) != 2 || !strings.Contains(html, `alt="The image at /avatar"`) {
		t.Fatalf("html shows %d images", strings.Count(html, `<img src="data:image/`))
	}
	o := goldenOptions()
	o.NoImages = true
	if off := render(t, HTML, o, rs); strings.Contains(off, `<img src="data:image/`) || !strings.Contains(off, "<code>/avatar</code>") {
		t.Fatal("images off: still embedded, or not listed")
	}
	pdf := render(t, PDF, goldenOptions(), rs)
	if n := strings.Count(pdf, "/Subtype /Image"); n < 2 {
		t.Fatalf("%d images in the PDF", n)
	}
	if n := strings.Count(render(t, PDF, o, rs), "/Subtype /Image"); n != 0 {
		t.Fatalf("images off: %d images in the PDF", n)
	}
}

// A deeply nested body keeps every report format small.
func TestReportOfADeepBody(t *testing.T) {
	const depth = 9999
	body := strings.Repeat("[", depth) + "1" + strings.Repeat("]", depth)
	req := store.Request{ID: 1, No: 1, Method: "POST", Path: "/deep", Status: 200, ReceivedAt: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC),
		Headers: []store.Header{{Name: "Content-Type", Value: "application/json"}}, Body: []byte(body), Size: int64(len(body))}
	o := goldenOptions()
	o.BodyLimit = 64 << 10
	for _, f := range []Format{Markdown, HTML, PDF} {
		if out := render(t, f, o, []store.Request{req}); len(out) > 4<<20 {
			t.Errorf("%s: %d MB", f, len(out)>>20)
		}
	}
}

// A body not kept because the server was busy, and one of which only the
// head was kept, say so.
func TestReportBodyNotes(t *testing.T) {
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	busy := store.Request{ID: 1, No: 1, Method: "POST", Path: "/up", ReceivedAt: at, Size: 70000, TooLarge: true, Status: 503,
		Outcome: emulate.Outcome{Source: emulate.FromBusy, Action: emulate.ActReply, Status: 503}.String()}
	head := store.Request{ID: 2, No: 2, Method: "POST", Path: "/nowhere", ReceivedAt: at, Size: 200 << 10, Status: 404,
		Body: []byte(strings.Repeat("x", 64<<10))}
	md := render(t, Markdown, goldenOptions(), []store.Request{busy, head})
	for _, want := range []string{"not kept: the server was busy (503)", "the first 64.0 KiB kept (no endpoint)"} {
		if !strings.Contains(md, want) {
			t.Errorf("lacks %q:\n%s", want, md)
		}
	}
}

// A request that arrives while the report is written is in neither its
// summary nor its items: the items stop at the last request counted.
func TestReportItemsMatchTheSummary(t *testing.T) {
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	rs := []store.Request{
		{ID: 1, No: 1, Method: "POST", Path: "/o", ReceivedAt: at, Status: 200},
		{ID: 2, No: 2, Method: "POST", Path: "/o", ReceivedAt: at.Add(time.Second), Status: 200},
	}
	var bodyPass int64 = -1
	src := func(ctx context.Context, withBody bool, through int64, fn func(store.Request) error) error {
		if withBody {
			bodyPass = through
		}
		for _, r := range rs {
			if through > 0 && r.ID > through {
				continue
			}
			if err := fn(r); err != nil {
				return err
			}
		}
		if !withBody { // one more arrives between the passes
			rs = append(rs, store.Request{ID: 3, No: 3, Method: "POST", Path: "/o", ReceivedAt: at.Add(2 * time.Second), Status: 500})
		}
		return nil
	}
	var buf bytes.Buffer
	if err := Write(context.Background(), &buf, Markdown, goldenOptions(), src); err != nil {
		t.Fatal(err)
	}
	md := buf.String()
	if bodyPass != 2 || !strings.Contains(md, "| Requests | 2 |") || strings.Contains(md, "### #3") || !strings.Contains(md, "### #2") {
		t.Fatalf("items bounded by %d:\n%s", bodyPass, md)
	}
}
