package schema

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

type wantProblem struct {
	Path, Keyword   string
	MessageContains string `json:"message_contains"`
}

type fixture struct {
	Name         string
	Schemas      []Doc
	Schema       string
	Body         string
	CompileError string `json:"compile_error"`
	Want         struct {
		OK       bool
		Total    int
		Count    int
		Problems []wantProblem
	}
}

func TestFixtures(t *testing.T) {
	b, err := os.ReadFile("../../testdata/schema/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []fixture
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			set, err := Compile(c.Schemas)
			if c.CompileError != "" {
				if err == nil || !strings.Contains(err.Error(), c.CompileError) {
					t.Fatalf("compile error %v, want one containing %q", err, c.CompileError)
				}
				return
			}
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			got := set.Validate(c.Schema, []byte(c.Body))
			if got.OK != c.Want.OK {
				t.Fatalf("ok %v, want %v: %+v", got.OK, c.Want.OK, got.Problems)
			}
			if c.Want.OK {
				return
			}
			if got.Total != c.Want.Total {
				t.Fatalf("total %d, want %d: %+v", got.Total, c.Want.Total, got.Problems)
			}
			if c.Want.Count > 0 && len(got.Problems) != c.Want.Count {
				t.Fatalf("%d problems listed, want %d", len(got.Problems), c.Want.Count)
			}
			for _, w := range c.Want.Problems {
				found := false
				for _, p := range got.Problems {
					if p.Path == w.Path && p.Keyword == w.Keyword && strings.Contains(p.Message, w.MessageContains) && p.Message != "" {
						found = true
					}
				}
				if !found {
					t.Fatalf("no problem %+v in %+v", w, got.Problems)
				}
			}
		})
	}
}

func TestCachedReusesTheSet(t *testing.T) {
	docs := []Doc{{Name: "A", Text: `{"type":"object"}`}}
	a, err1 := Cached(docs)
	b, err2 := Cached([]Doc{{Name: "A", Text: `{"type":"object"}`}})
	c, _ := Cached([]Doc{{Name: "A", Text: `{"type":"array"}`}})
	if err1 != nil || err2 != nil || a != b || a == c {
		t.Fatalf("cache: %p %p %p %v %v", a, b, c, err1, err2)
	}
	if _, err := Cached([]Doc{{Name: "B", Text: `{"type":5}`}}); err == nil {
		t.Fatal("a compile error is not remembered as success")
	}
}

// TestNoFileIsRead: a $ref to a readable file holding a valid schema is
// refused; the library's default loader would read it.
func TestNoFileIsRead(t *testing.T) {
	path := t.TempDir() + "/valid.json"
	if err := os.WriteFile(path, []byte(`{"type":"string"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Compile([]Doc{{Name: "Local", Text: `{"$ref":"file://` + path + `"}`}})
	if err == nil || !strings.Contains(err.Error(), "never fetched") {
		t.Fatalf("a $ref to a local file compiled: %v", err)
	}
}

type wantImage struct {
	Path, Type    string
	Width, Height int
	ErrorContains string `json:"error_contains"`
}

func TestImageFixtures(t *testing.T) {
	b, err := os.ReadFile("../../testdata/schema/images.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		fixture
		Images []wantImage
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			set, err := Compile(c.Schemas)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			got := set.Validate(c.Schema, []byte(c.Body))
			if got.OK != c.Want.OK {
				t.Fatalf("ok %v, want %v: %+v", got.OK, c.Want.OK, got.Problems)
			}
			if !c.Want.OK && got.Total != c.Want.Total {
				t.Fatalf("total %d, want %d: %+v", got.Total, c.Want.Total, got.Problems)
			}
			for _, w := range c.Want.Problems {
				found := false
				for _, p := range got.Problems {
					if p.Path == w.Path && p.Keyword == w.Keyword && strings.Contains(p.Message, w.MessageContains) {
						found = true
					}
				}
				if !found {
					t.Fatalf("no problem %+v in %+v", w, got.Problems)
				}
			}
			if len(got.Images) != len(c.Images) {
				t.Fatalf("%d images, want %d: %+v", len(got.Images), len(c.Images), got.Images)
			}
			for i, w := range c.Images {
				g := got.Images[i]
				if g.Path != w.Path || g.Type != w.Type || g.Width != w.Width || g.Height != w.Height ||
					(w.ErrorContains == "") != (g.Error == "") || !strings.Contains(g.Error, w.ErrorContains) {
					t.Fatalf("image %d: %+v, want %+v", i, g, w)
				}
				if g.Error == "" && (len(g.SHA256) != 64 || g.Size == 0) {
					t.Fatalf("image %d: no size or digest: %+v", i, g)
				}
			}
		})
	}
}

// TestProblemsInOneOrder: the validator walks a schema's properties in map
// order, so problems are sorted (by place in the body, numbers as numbers,
// then keyword) before the MaxProblems cut: the same body gives the same
// list every time.
func TestProblemsInOneOrder(t *testing.T) {
	props, body := []string{}, []string{}
	for i := 0; i < 30; i++ {
		props = append(props, fmt.Sprintf(`"k%02d":{"type":"string"}`, i))
		body = append(body, fmt.Sprintf(`"k%02d":%d`, i, i))
	}
	text := `{"type":"object","properties":{` + strings.Join(props, ",") +
		`,"list":{"type":"array","items":{"type":"integer","minimum":5}}},"required":["absent"]}`
	set, err := Compile([]Doc{{Name: "S", Text: text}})
	if err != nil {
		t.Fatal(err)
	}
	doc := []byte(`{` + strings.Join(body, ",") + `,"list":[1,2,3,4,0,1,2,3,4,0,1]}`)
	first := set.Validate("S", doc)
	if first.Total != 30+11+1 || len(first.Problems) != MaxProblems {
		t.Fatalf("total %d, kept %d", first.Total, len(first.Problems))
	}
	want := []string{"", "/k00", "/k01"} // (body) required first, then in order
	for i, w := range want {
		if first.Problems[i].Path != w {
			t.Fatalf("problem %d at %q, want %q: %v", i, first.Problems[i].Path, w, first.Problems)
		}
	}
	if last := first.Problems[MaxProblems-1].Path; last != "/k18" {
		t.Fatalf("the cut kept up to %q, want /k18 (sorted before the cut)", last)
	}
	for range 50 {
		again := set.Validate("S", doc)
		if fmt.Sprint(again.Problems) != fmt.Sprint(first.Problems) {
			t.Fatalf("another order:\n%v\n%v", again.Problems, first.Problems)
		}
	}
	// Array indexes compare as numbers: /list/2 before /list/10.
	all := set.Validate("S", []byte(`{"list":[1,2,3,4,0,1,2,3,4,0,1]}`))
	var got []string
	for _, p := range all.Problems {
		got = append(got, p.Path)
	}
	if strings.Join(got, " ") != " /list/0 /list/1 /list/2 /list/3 /list/4 /list/5 /list/6 /list/7 /list/8 /list/9 /list/10" {
		t.Fatalf("order %q", got)
	}
	// Two problems at one place: by keyword path.
	two, err := Compile([]Doc{{Name: "T", Text: `{"properties":{"code":{"type":"string","pattern":"^X","minLength":5,"maxLength":1}}}`}})
	if err != nil {
		t.Fatal(err)
	}
	for range 30 {
		var kws []string
		for _, p := range two.Validate("T", []byte(`{"code":"abc"}`)).Problems {
			kws = append(kws, p.Keyword)
		}
		if strings.Join(kws, " ") != "maxLength minLength pattern" {
			t.Fatalf("keywords at one place in order %q, want maxLength minLength pattern", kws)
		}
	}
}
