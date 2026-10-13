package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
// data URL — is asserted to be such an image: the library calls these
// media types and the decoder below while validating, so a broken image is
// a problem at its JSON pointer like any other. A second walk over the
// schema and the body then records every declared image (findImages).

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
			_, err := picture.Verify(b, t)
			return err
		}})
	}
}

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
func findImages(sch *jsonschema.Schema, inst any) []Image {
	found := map[string]Image{}
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
				found[ptr] = inspect(ptr, str, s.ContentMediaType.Name, s.ContentEncoding != nil)
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
	out := make([]Image, 0, len(found))
	for _, img := range found {
		out = append(out, img)
	}
	slices.SortFunc(out, func(a, b Image) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// inspect decodes one declared image and says what it is. base64 tells
// whether the schema declares the encoding; without it only a data URL
// carries an image.
func inspect(path, value, declared string, base64 bool) Image {
	img := Image{Path: path, Declared: declared}
	if !base64 && !picture.IsDataImage(value) {
		img.Error = "not a data URL, and the schema declares no contentEncoding base64"
		return img
	}
	e, err := picture.Decode(value)
	if err != nil {
		img.Error = err.Error()
		return img
	}
	img.Size = len(e.Bytes)
	sum := sha256.Sum256(e.Bytes)
	img.SHA256 = hex.EncodeToString(sum[:])
	info, err := picture.Verify(e.Bytes, declared)
	img.Type, img.Width, img.Height = info.Type, info.Width, info.Height
	switch {
	case err != nil:
		img.Error = err.Error()
	case e.URLType != "" && e.URLType != info.Type:
		img.Error = fmt.Sprintf("the data URL says %s, the data is %s", e.URLType, info.Type)
	}
	return img
}

// dataURLProblems are the images whose data URL names another type than
// their data: the library's checks do not see the URL's own type.
func dataURLProblems(imgs []Image) []Problem {
	var out []Problem
	for _, img := range imgs {
		if strings.HasPrefix(img.Error, "the data URL says") {
			out = append(out, Problem{Path: img.Path, Keyword: "contentMediaType", Message: img.Error})
		}
	}
	return out
}

// Undecoded counts the images recognised but not decodable here (HEIC,
// AVIF): the check cannot say they are whole.
func Undecoded(imgs []Image) int {
	n := 0
	for _, img := range imgs {
		if img.Error == "" && img.Type != "" && !picture.Decodable(img.Type) {
			n++
		}
	}
	return n
}
