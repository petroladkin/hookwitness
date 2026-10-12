package schema

import (
	"encoding/json"
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
