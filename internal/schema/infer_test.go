package schema

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestDraftCases runs testdata/schema/drafts.json.
func TestDraftCases(t *testing.T) {
	b, err := os.ReadFile("../../testdata/schema/drafts.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name   string
		Bodies []string
		Want   any
		Error  string
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			var bodies [][]byte
			for _, s := range tc.Bodies {
				bodies = append(bodies, []byte(s))
			}
			text, err := Draft(bodies)
			if tc.Error != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tc.Error) {
					t.Fatalf("error %v, want %q…", err, tc.Error)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got any
			if err := json.Unmarshal([]byte(text), &got); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, text)
			}
			if !reflect.DeepEqual(got, tc.Want) {
				want, _ := json.MarshalIndent(tc.Want, "", "  ")
				t.Fatalf("draft\n%s\nwant\n%s", text, want)
			}
			if !validatesAll(text, bodies) {
				t.Fatal("the draft does not validate its own bodies")
			}
		})
	}
}

// TestDraftValidatesItsBodies: whatever the bodies, the draft compiles and
// validates every one of them.
func TestDraftValidatesItsBodies(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	strs := []string{"", "x", "2026-10-12T17:28:07Z", "2026-10-12", "2026-02-30", "ann@example.com", "a@b",
		"https://example.com/x", "mailto:ann@example.com", "0b9f8a2e-3c1d-4e5f-8a7b-9c0d1e2f3a4b", "ü ", "1e3"}
	keys := []string{"a", "b", "type", "a/b", "~", "$ref", ""}
	var gen func(depth int) any
	gen = func(depth int) any {
		switch k := r.IntN(8); {
		case k == 0:
			return nil
		case k == 1:
			return r.IntN(2) == 0
		case k == 2:
			return json.Number(fmt.Sprint(r.IntN(2000) - 1000))
		case k == 3:
			return json.Number(fmt.Sprintf("%d.%d", r.IntN(100), r.IntN(10)))
		case k == 4 || depth > 3:
			return strs[r.IntN(len(strs))]
		case k == 5:
			arr := []any{}
			for range r.IntN(4) {
				arr = append(arr, gen(depth+1))
			}
			return arr
		default:
			obj := map[string]any{}
			for range r.IntN(5) {
				obj[keys[r.IntN(len(keys))]] = gen(depth + 1)
			}
			return obj
		}
	}
	for i := range 400 {
		var bodies [][]byte
		for range 1 + r.IntN(3) {
			b, _ := json.Marshal(gen(0))
			bodies = append(bodies, b)
		}
		text, err := Draft(bodies)
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		if !validatesAll(text, bodies) {
			t.Fatalf("%d: the draft does not validate its bodies\n%s\n%s", i, text, bodies)
		}
	}
}

func TestChanges(t *testing.T) {
	stored, _ := Draft([][]byte{[]byte(`{"id":1,"at":"2026-10-12T17:28:07Z","tags":["a"],"addr":{"zip":"01001"},"old":true}`)})
	draft, _ := Draft([][]byte{[]byte(`{"id":1.5,"at":"later","tags":[1],"addr":{"zip":"01001","city":"Kyiv"},"note":null}`),
		[]byte(`{"id":2,"at":"x","tags":[],"addr":{}}`)})
	got, err := Changes(stored, draft)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/addr/city: new key, optional",
		"/addr/zip: now optional",
		"/at: format date-time dropped",
		"/id: type integer → number",
		"/note: new key, optional",
		"/old: not in the draft (not seen in these requests)",
		"/tags/items/*: type string → integer",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("changes\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if same, _ := Changes(stored, stored); len(same) != 0 {
		t.Fatalf("no changes: %v", same)
	}
	if _, err := Changes("[1]", draft); err == nil {
		t.Fatal("a stored schema that is not an object")
	}
}
