package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/petroladkin/hookwitness/internal/auth"
	"github.com/petroladkin/hookwitness/internal/picture"
)

// Image is a picture a schema declares in a body, as found.
type Image = auth.Image

// Images in schemas: a string with contentMediaType image/png (jpeg, gif,
// webp, heic, avif, or image/* for any) and contentEncoding base64 — or a
// data URL — is asserted to be such an image. The library calls these media
// types and the decoder below while validating, as often as its branches
// need, so they check the header only: a broken one is a problem at its
// JSON pointer like any other. A second walk over the schema and the body
// then records every declared image and decodes each once, within a budget
// (findImages); an image whose pixels do not decode is a problem too.

// assertImages registers the image media types and a base64 decoder that
// also takes data URLs.
func assertImages(c *jsonschema.Compiler) {
	c.AssertContent()
	c.RegisterContentEncoding(&jsonschema.Decoder{Name: "base64", Decode: func(s string) ([]byte, error) {
		e, err := picture.Decode(s)
		return e.Bytes, err
	}})
	for _, t := range picture.Declarable {
		c.RegisterContentMediaType(&jsonschema.MediaType{Name: t, Validate: func(b []byte) error {
			if strings.HasPrefix(string(b[:min(len(b), 5)]), "data:") { // a data URL without contentEncoding
				e, err := picture.Decode(string(b))
				if err != nil {
					return err
				}
				b = e.Bytes
			}
			_, err := picture.Check(b, t)
			return err
		}})
	}
}

// imageBudget is the decoding one validation may do (a variable for tests).
var imageBudget int64 = picture.RequestBudget

// isImageType reports a contentMediaType this package treats as an image.
func isImageType(mt *jsonschema.MediaType) bool {
	return mt != nil && slices.Contains(picture.Declarable, mt.Name)
}

// findImages walks the schema and the instance together and records every
// string the schema declares as an image, in path order. It follows the
// applicators a payload schema uses: $ref, allOf, the anyOf / oneOf
// branches and the if / then / else branch the value takes, properties,
// patternProperties, additionalProperties, dependentSchemas, items and
// prefixItems.
func findImages(sch *jsonschema.Schema, inst any) ([]Image, []Problem) {
	type candidate struct {
		value, declared string
		base64          bool
	}
	found := map[string]candidate{}
	seen := map[*jsonschema.Schema]map[string]bool{} // guards recursive schemas
	var walk func(s *jsonschema.Schema, v any, path []string, depth int)
	walk = func(s *jsonschema.Schema, v any, path []string, depth int) {
		if s == nil || depth > 64 {
			return
		}
		ptr := pointer(path)
		if seen[s] == nil {
			seen[s] = map[string]bool{}
		}
		if seen[s][ptr] {
			return
		}
		seen[s][ptr] = true

		if str, ok := v.(string); ok && isImageType(s.ContentMediaType) {
			if _, done := found[ptr]; !done {
				found[ptr] = candidate{str, s.ContentMediaType.Name, s.ContentEncoding != nil}
			}
		}
		walk(s.Ref, v, path, depth+1)
		walk(s.RecursiveRef, v, path, depth+1)
		if s.DynamicRef != nil {
			walk(s.DynamicRef.Ref, v, path, depth+1)
		}
		for _, sub := range s.AllOf {
			walk(sub, v, path, depth+1)
		}
		for _, sub := range append(slices.Clone(s.AnyOf), s.OneOf...) {
			if sub.Validate(v) == nil {
				walk(sub, v, path, depth+1)
			}
		}
		if s.If != nil {
			if s.If.Validate(v) == nil {
				walk(s.Then, v, path, depth+1)
			} else {
				walk(s.Else, v, path, depth+1)
			}
		}
		switch v := v.(type) {
		case map[string]any:
			for k, val := range v {
				p := append(slices.Clone(path), k)
				matched := false
				if sub, ok := s.Properties[k]; ok {
					walk(sub, val, p, depth+1)
					matched = true
				}
				for re, sub := range s.PatternProperties {
					if re.MatchString(k) {
						walk(sub, val, p, depth+1)
						matched = true
					}
				}
				if add, ok := s.AdditionalProperties.(*jsonschema.Schema); ok && !matched {
					walk(add, val, p, depth+1)
				}
				if sub, ok := s.DependentSchemas[k]; ok {
					walk(sub, v, path, depth+1)
				}
			}
		case []any:
			for i, val := range v {
				p := append(slices.Clone(path), strconv.Itoa(i))
				switch {
				case i < len(s.PrefixItems):
					walk(s.PrefixItems[i], val, p, depth+1)
				case s.Items2020 != nil:
					walk(s.Items2020, val, p, depth+1)
				}
				switch items := s.Items.(type) {
				case *jsonschema.Schema:
					walk(items, val, p, depth+1)
				case []*jsonschema.Schema:
					if i < len(items) {
						walk(items[i], val, p, depth+1)
					} else if add, ok := s.AdditionalItems.(*jsonschema.Schema); ok {
						walk(add, val, p, depth+1)
					}
				}
			}
		}
	}
	walk(sch, inst, nil, 0)
	// In path order, so the budget goes to the same images every time.
	paths := slices.Sorted(maps.Keys(found))
	budget := picture.NewBudget(imageBudget)
	out := make([]Image, 0, len(paths))
	var probs []Problem
	for _, p := range paths {
		c := found[p]
		img, new := inspect(p, c.value, c.declared, c.base64, budget)
		out = append(out, img)
		if new {
			probs = append(probs, Problem{Path: p, Keyword: "contentMediaType", Message: img.Error})
		}
	}
	return out, probs
}

// inspect decodes one declared image and says what it is. base64 tells
// whether the schema declares the encoding; without it only a data URL
// carries an image. problem reports an error the library's own check does
// not see: pixels that do not decode, or a data URL naming another type.
func inspect(path, value, declared string, base64 bool, budget *picture.Budget) (img Image, problem bool) {
	img = Image{Path: path, Declared: declared}
	if !base64 && !picture.IsDataImage(value) {
		img.Error = "not a data URL, and the schema declares no contentEncoding base64"
		return img, false
	}
	e, err := picture.Decode(value)
	if err != nil {
		img.Error = err.Error()
		return img, false
	}
	img.Size = len(e.Bytes)
	sum := sha256.Sum256(e.Bytes)
	img.SHA256 = hex.EncodeToString(sum[:])
	headerErr := false
	info, notChecked, err := budget.Verify(e.Bytes, declared)
	if err != nil {
		_, hdr := picture.Check(e.Bytes, declared)
		headerErr = hdr != nil // the library has reported it already
	}
	img.Type, img.Width, img.Height, img.NotChecked = info.Type, info.Width, info.Height, notChecked
	switch {
	case err != nil:
		img.Error = err.Error()
		return img, !headerErr
	case e.URLType != "" && e.URLType != info.Type:
		img.Error = fmt.Sprintf("the data URL says %s, the data is %s", e.URLType, info.Type)
		return img, true
	}
	return img, false
}

// Unchecked counts the images whose pixels were not checked: recognised but
// not decodable here (HEIC, AVIF), or over the decoding limits. The check
// cannot say they are whole.
func Unchecked(imgs []Image) int {
	n := 0
	for _, img := range imgs {
		if img.Error == "" && img.Type != "" && (!picture.Decodable(img.Type) || img.NotChecked != "") {
			n++
		}
	}
	return n
}

// WhyUnchecked is the reason the first image over the decoding limits gives,
// "" when none is.
func WhyUnchecked(imgs []Image) string {
	for _, img := range imgs {
		if img.Error == "" && img.NotChecked != "" {
			return img.NotChecked
		}
	}
	return ""
}
