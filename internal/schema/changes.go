package schema

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Changes lists how a draft differs from a stored schema of the same name,
// in what a draft can say: keys added or not seen, required or optional,
// types and formats. Paths are property paths, /items/* for array items.
// It compares only what drafts write; a hand-written schema's other
// keywords (enum, pattern, $ref, oneOf...) are not compared.
func Changes(stored, draft string) ([]string, error) {
	var a, b map[string]any
	if err := json.Unmarshal([]byte(stored), &a); err != nil {
		return nil, fmt.Errorf("the stored schema is not a JSON object: %v", err)
	}
	if err := json.Unmarshal([]byte(draft), &b); err != nil {
		return nil, fmt.Errorf("the draft is not a JSON object: %v", err)
	}
	var out []string
	compare(&out, "", a, b)
	return out, nil
}

func compare(out *[]string, path string, a, b map[string]any) {
	where := path
	if where == "" {
		where = "(body)"
	}
	if ta, tb := typeList(a), typeList(b); !slices.Equal(ta, tb) {
		switch {
		case len(ta) == 0:
			*out = append(*out, fmt.Sprintf("%s: type %s (any before)", where, strings.Join(tb, ", ")))
		case len(tb) == 0:
			*out = append(*out, fmt.Sprintf("%s: any type (%s before)", where, strings.Join(ta, ", ")))
		default:
			*out = append(*out, fmt.Sprintf("%s: type %s → %s", where, strings.Join(ta, ", "), strings.Join(tb, ", ")))
		}
	}
	if fa, fb := str(a["format"]), str(b["format"]); fa != fb {
		switch {
		case fa == "":
			*out = append(*out, fmt.Sprintf("%s: format %s added", where, fb))
		case fb == "":
			*out = append(*out, fmt.Sprintf("%s: format %s dropped", where, fa))
		default:
			*out = append(*out, fmt.Sprintf("%s: format %s → %s", where, fa, fb))
		}
	}
	pa, _ := a["properties"].(map[string]any)
	pb, _ := b["properties"].(map[string]any)
	ra, rb := strs(a["required"]), strs(b["required"])
	for _, k := range keys(pb, pa) {
		p := path + "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(k)
		sa, inA := pa[k].(map[string]any)
		sb, inB := pb[k].(map[string]any)
		switch {
		case !inA && pa[k] == nil && inB:
			*out = append(*out, p+": new key"+optional(slices.Contains(rb, k)))
			continue
		case !inB && pb[k] == nil:
			*out = append(*out, p+": not in the draft (not seen in these requests)")
			continue
		}
		if was, is := slices.Contains(ra, k), slices.Contains(rb, k); was != is {
			if is {
				*out = append(*out, p+": now required")
			} else {
				*out = append(*out, p+": now optional")
			}
		}
		if inA && inB {
			compare(out, p, sa, sb)
		}
	}
	ia, _ := a["items"].(map[string]any)
	ib, _ := b["items"].(map[string]any)
	if ia != nil && ib != nil {
		compare(out, path+"/items/*", ia, ib)
	}
}

func optional(required bool) string {
	if required {
		return ", required"
	}
	return ", optional"
}

// keys are the draft's keys, then the stored schema's others, sorted.
func keys(draft, stored map[string]any) []string {
	var out []string
	for k := range draft {
		out = append(out, k)
	}
	for k := range stored {
		if _, ok := draft[k]; !ok {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func typeList(m map[string]any) []string {
	var out []string
	switch t := m["type"].(type) {
	case string:
		out = []string{t}
	case []any:
		out = strs(t)
	}
	slices.Sort(out)
	return out
}

func str(v any) string { s, _ := v.(string); return s }

func strs(v any) []string {
	l, _ := v.([]any)
	var out []string
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
