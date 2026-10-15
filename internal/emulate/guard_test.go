package emulate

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// allocated is the bytes allocated while f runs (all goroutines).
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// Templates that would build gigabytes are refused quickly and allocate
// little: the string functions are bounded before they build.
func TestTemplateBombs(t *testing.T) {
	d := NewData(Request{Method: "POST", Path: "/a", Body: []byte(`{"x":1}`)}, nil, 1)
	double := `{{$a := printf "%1000000d" 0}}` + strings.Repeat(`{{$a = printf "%s%s" $a $a}}`, 12)
	chain := `{{$a := printf "%500000d" 0}}`
	for i := range 40 {
		chain += `{{$b` + string(rune('a'+i%26)) + itoa(i) + ` := printf "%s%s" $a $a}}`
	}
	cases := map[string]string{
		"reassignment (the review's)":    double,
		"new variables, no reassignment": chain,
		"repeated argument":              `{{$a := printf "%900000d" 0}}{{printf "%[1]s%[1]s%[1]s%[1]s" $a}}`,
		"width from an argument":         `{{printf "%*d" 900000000 1}}`,
		"literal width":                  `{{printf "%2000000d" 1}}`,
		"html growth":                    `{{$a := printf "%900000s" "<"}}{{$b := html $a}}{{$c := html $b}}`,
		"print":                          `{{$a := printf "%900000d" 0}}{{print $a $a $a $a $a $a}}`,
		"budget over many variables":     `{{$a := printf "%900000d" 0}}` + strings.Repeat(`{{$z := print $a}}`, 10),
	}
	for name, src := range cases {
		var err error
		start := time.Now()
		bytes := allocated(func() { _, err = (Reply{Body: src}).Render(d) })
		if err == nil {
			t.Errorf("%s: rendered", name)
			continue
		}
		if !strings.Contains(err.Error(), "could build more than") && !strings.Contains(err.Error(), "in all") &&
			!strings.Contains(err.Error(), "over 1048576 bytes") {
			t.Errorf("%s: %v", name, err)
		}
		if bytes > 64<<20 {
			t.Errorf("%s: allocated %d MB", name, bytes>>20)
		}
		if took := time.Since(start); took > renderTimeout {
			t.Errorf("%s: took %s", name, took)
		}
	}
}

func itoa(i int) string { return strings.Repeat("x", i/26) }

// Ordinary uses of the string functions still work.
func TestTemplateFunctions(t *testing.T) {
	d := NewData(Request{Method: "POST", Path: "/a", RawQuery: "q=a b", Body: []byte(`{"id":"e<1>"}`)}, nil, 7)
	for src, want := range map[string]string{
		`{{printf "%s-%04d" (.JSON "id") .N}}`: "e<1>-0007",
		`{{printf "%[1]s/%[1]s" .Method}}`:     "POST/POST",
		`{{printf "%*d" 5 .N}}`:                "    7",
		`{{printf "%q" .Path}}`:                `"/a"`,
		`{{print .Method " " .N}}`:             "POST 7",
		`{{println .N}}`:                       "7\n",
		`{{html (.JSON "id")}}`:                "e&lt;1&gt;",
		`{{js (.JSON "id")}}`:                  `e\u003C1\u003E`,
		`{{urlquery (.Query "q")}}`:            "a+b",
		`{{printf "%x" .Method}}`:              "504f5354",
	} {
		out, err := (Reply{Body: src}).Render(d)
		if err != nil || string(out.Body) != want {
			t.Errorf("%s: %q, %v; want %q", src, out.Body, err, want)
		}
	}
	// Signed content may be as large as the body plus a reply.
	body := []byte(strings.Repeat("a", 3<<20))
	out, err := renderUpTo(`{{printf "%s.%s" (.Header "T") .Body}}`, NewData(Request{Body: body}, nil, 0), len(body)+maxRendered)
	if err != nil || len(out) != len(body)+1 {
		t.Errorf("signed content: %d bytes, %v", len(out), err)
	}
}

// A bound always holds: whatever fmt writes is within printfBound.
func TestPrintfBound(t *testing.T) {
	long := strings.Repeat("é\"<", 100)
	for _, c := range []struct {
		f    string
		args []any
	}{
		{"%s", []any{long}}, {"%q", []any{long}}, {"%+q", []any{long}}, {"% x", []any{long}},
		{"%X", []any{[]byte(long)}}, {"%v", []any{[]byte(long)}}, {"%#v", []any{map[string]string{"a": long}}},
		{"%[2]s%[1]s%[2]s", []any{long, "b"}}, {"%*.*f", []any{30, 10, 3.5}}, {"%-30s|%5.2d", []any{"a", 7}},
		{"%d %s", []any{1}}, {"%!", nil}, {"%", nil}, {"%[9]s", []any{"a"}}, {"%.*s", []any{"x", long}},
		{"%v", []any{nil}}, {"%t %c %U", []any{true, 'é', 'é'}},
	} {
		got := len(fmt.Sprintf(c.f, c.args...))
		if b := printfBound(c.f, c.args); got > b {
			t.Errorf("%q: wrote %d, bound %d", c.f, got, b)
		}
	}
}

// A render cut off by its deadline stops at its next action instead of
// running on, and a render does not race with the next on the same data.
func TestRenderStops(t *testing.T) {
	g := newGuard(maxRendered)
	g.stopped.Store(true)
	if _, err := g.funcs()["print"].(func(...any) (string, error))("a"); err != errStopped {
		t.Errorf("print after the deadline: %v", err)
	}
	if _, err := (&capped{limit: 10, g: g}).Write([]byte("a")); err != errStopped {
		t.Errorf("write after the deadline: %v", err)
	}
	d := NewData(Request{Body: []byte(`{"a":{"b":"c"}}`)}, nil, 0)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, err := (Reply{Body: `{{.JSON "a.b"}}`}).Render(d); err != nil || string(out.Body) != "c" {
				t.Errorf("%q, %v", out.Body, err)
			}
		}()
	}
	wg.Wait()
}
