package picture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/petroladkin/hookwitness/internal/auth"
)

// Found is an image a request carries, wherever it was found.
type Found struct {
	auth.Image
	Source string // "schema" (declared by a schema check), "data URL" (seen without one), "body"
}

// Body is the Path of an image that is the whole body.
const Body = ""

// Find lists the images of a request: those its schema verdicts record,
// plain data:image/... strings in a JSON body that no schema declared, and
// the body itself when it is an image. Paths are unique; the order is the
// body first, then by path.
func Find(body []byte, contentType string, verdicts []auth.Verdict) []Found {
	byPath := map[string]Found{}
	for _, v := range verdicts {
		for _, img := range v.Images {
			if _, ok := byPath[img.Path]; !ok {
				byPath[img.Path] = Found{Image: img, Source: "schema"}
			}
		}
	}
	if t := Sniff(body); t != "" || strings.HasPrefix(strings.ToLower(contentType), "image/") {
		byPath[Body] = Found{Image: Describe(Body, body, ""), Source: "body"}
	} else if json.Valid(body) {
		dataURLs(body, func(path, s string) {
			if _, ok := byPath[path]; ok {
				return
			}
			byPath[path] = Found{Image: describeString(path, s), Source: "data URL"}
		})
	}
	out := make([]Found, 0, len(byPath))
	for _, f := range byPath {
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b Found) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// Describe inspects decoded image bytes: type, size, dimensions, digest,
// and the reason it is not a valid image (of the declared type when given).
// The full check is remembered by digest, so showing a request again does
// not decode its images again; images a schema declared are not decoded
// here at all (Find takes them from the verdicts).
func Describe(path string, b []byte, declared string) auth.Image {
	img := auth.Image{Path: path, Declared: declared, Size: len(b)}
	sum := sha256.Sum256(b)
	img.SHA256 = hex.EncodeToString(sum[:])
	if declared == "" {
		declared = Any
	}
	v := verifyViewed(b, sum)
	info, err := v.info, v.err
	if err == nil && declared != Any && declared != info.Type {
		err = fmt.Errorf("a %s, declared %s", short(info.Type), short(declared))
	}
	img.Type, img.Width, img.Height, img.NotChecked = info.Type, info.Width, info.Height, v.notChecked
	if err != nil {
		img.Error = err.Error()
	}
	return img
}

func describeString(path, s string) auth.Image {
	e, err := Decode(s)
	if err != nil {
		return auth.Image{Path: path, Error: err.Error()}
	}
	img := Describe(path, e.Bytes, "")
	if img.Error == "" && e.URLType != img.Type {
		img.Error = fmt.Sprintf("the data URL says %s, the data is %s", e.URLType, img.Type)
	}
	return img
}

// dataURLs calls fn with the JSON pointer of every string in a JSON
// document that is an image data URL.
func dataURLs(body []byte, fn func(path, s string)) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var walk func(path string) error
	walk = func(path string) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				for dec.More() {
					k, err := dec.Token()
					if err != nil {
						return err
					}
					key, _ := k.(string)
					if err := walk(path + "/" + escape(key)); err != nil {
						return err
					}
				}
			case '[':
				for i := 0; dec.More(); i++ {
					if err := walk(path + "/" + strconv.Itoa(i)); err != nil {
						return err
					}
				}
			}
			_, err := dec.Token() // the closing delimiter
			return err
		case string:
			if IsDataImage(t) {
				fn(path, t)
			}
		}
		return nil
	}
	if err := walk(""); err != nil && !errors.Is(err, io.EOF) {
		return
	}
}

func escape(k string) string { return strings.NewReplacer("~", "~0", "/", "~1").Replace(k) }

// At returns the decoded bytes of the image at a JSON pointer in a body
// (Body for the body itself).
func At(body []byte, path string) ([]byte, error) {
	if path == Body {
		return body, nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil, errors.New("not a JSON pointer")
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, errors.New("the body is not JSON")
	}
	for _, tok := range strings.Split(path[1:], "/") {
		tok = strings.NewReplacer("~1", "/", "~0", "~").Replace(tok)
		switch c := v.(type) {
		case map[string]any:
			next, ok := c[tok]
			if !ok {
				return nil, errors.New("no value at " + path)
			}
			v = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(c) {
				return nil, errors.New("no value at " + path)
			}
			v = c[i]
		default:
			return nil, errors.New("no value at " + path)
		}
	}
	s, ok := v.(string)
	if !ok {
		return nil, errors.New("the value at " + path + " is not a string")
	}
	e, err := Decode(s)
	if err != nil {
		return nil, err
	}
	return e.Bytes, nil
}

// Ext is the file extension of a media type, "bin" when unknown.
func Ext(mediaType string) string {
	switch mediaType {
	case PNG:
		return "png"
	case JPEG:
		return "jpg"
	case GIF:
		return "gif"
	case WebP:
		return "webp"
	case HEIC:
		return "heic"
	case AVIF:
		return "avif"
	}
	return "bin"
}
