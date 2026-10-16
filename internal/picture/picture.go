// Package picture recognises and checks images that requests carry: base64
// strings and data URLs inside JSON, or whole bodies. It is pure: it looks
// at bytes, decodes PNG, JPEG, GIF and WebP fully so that a truncated or
// corrupt image is caught, and never renders, resizes or fetches anything.
// HEIC and AVIF are recognised by their signature but cannot be decoded
// here; SVG is not accepted as an image (it can carry scripts).
package picture

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // registers the decoder
	_ "image/jpeg"
	_ "image/png"
	"strings"

	_ "golang.org/x/image/webp"
)

// MaxPixels bounds an image's width × height (40 megapixels): a larger one
// is refused before it is decoded.
const MaxPixels = 40_000_000

// Decoding costs memory by the pixel and by the bytes each pixel takes: a
// small 16-bit PNG of 40 MP decodes to 320 MB. These bound it.
const (
	MaxDecodedBytes = 128 << 20 // one image: a larger one is not decoded, its check says so
	RequestBudget   = 256 << 20 // all images decoded for one validation
)

// The media types this package knows.
const (
	PNG  = "image/png"
	JPEG = "image/jpeg"
	GIF  = "image/gif"
	WebP = "image/webp"
	HEIC = "image/heic"
	AVIF = "image/avif"
	Any  = "image/*" // a schema's "any image"
)

// Declarable is every contentMediaType a schema may use for an image.
var Declarable = []string{PNG, JPEG, GIF, WebP, HEIC, AVIF, Any}

// Decodable reports a type this package decodes fully.
func Decodable(mediaType string) bool {
	switch mediaType {
	case PNG, JPEG, GIF, WebP:
		return true
	}
	return false
}

// Sniff names the image type of b by its signature, "" when it is none of
// the known ones.
func Sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return PNG
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return JPEG
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return GIF
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return WebP
	case len(b) >= 12 && string(b[4:8]) == "ftyp":
		switch string(b[8:12]) {
		case "avif", "avis":
			return AVIF
		case "heic", "heix", "hevc", "hevx", "mif1", "msf1":
			return HEIC
		}
	}
	return ""
}

// looksLikeSVG reports text that starts like an SVG document.
func looksLikeSVG(b []byte) bool {
	head := strings.ToLower(string(b[:min(len(b), 512)]))
	return strings.Contains(head, "<svg")
}

// Info is what is known of an image without keeping it.
type Info struct {
	Type          string // the sniffed media type
	Width, Height int    // 0 when the type cannot be decoded here
}

// Inspect sniffs b and reads its dimensions from the header; it does not
// decode the pixels. It fails when b is not a known image or is too large.
func Inspect(b []byte) (Info, error) {
	info, _, err := header(b)
	return info, err
}

// header is Inspect with the header's colour model, for DecodedBytes.
func header(b []byte) (Info, image.Config, error) {
	t := Sniff(b)
	if t == "" {
		if looksLikeSVG(b) {
			return Info{}, image.Config{}, errors.New("an SVG document, which is not accepted as an image")
		}
		return Info{}, image.Config{}, errors.New("not a PNG, JPEG, GIF, WebP, HEIC or AVIF image")
	}
	if !Decodable(t) {
		return Info{Type: t}, image.Config{}, nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return Info{Type: t}, cfg, fmt.Errorf("a broken %s header: %v", short(t), err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return Info{Type: t}, cfg, fmt.Errorf("a %s of %d×%d pixels", short(t), cfg.Width, cfg.Height)
	}
	info := Info{Type: t, Width: cfg.Width, Height: cfg.Height}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return info, cfg, fmt.Errorf("%d×%d pixels, over the limit of %d megapixels", cfg.Width, cfg.Height, MaxPixels/1_000_000)
	}
	return info, cfg, nil
}

// Check is Inspect plus the declared type (Any for any known one): what can
// be told from the header, without decoding the pixels.
func Check(b []byte, declared string) (Info, error) {
	info, err := Inspect(b)
	if err != nil {
		return info, err
	}
	if declared != Any && declared != info.Type {
		return info, fmt.Errorf("a %s, declared %s", short(info.Type), short(declared))
	}
	return info, nil
}

// Verify checks that b is an image of the declared type and, when it can be
// decoded here, that it decodes completely. One that would take more than
// MaxDecodedBytes to decode is not decoded: the error says so.
func Verify(b []byte, declared string) (Info, error) {
	info, notChecked, err := NewBudget(MaxDecodedBytes).Verify(b, declared)
	if err == nil && notChecked != "" {
		err = errors.New("not checked: " + notChecked)
	}
	return info, err
}

// short is a media type's subtype in capitals: "PNG".
func short(t string) string {
	return strings.ToUpper(strings.TrimPrefix(t, "image/"))
}

// Encoded is a string value decoded: its bytes and, for a data URL, the
// media type the URL itself declares.
type Encoded struct {
	Bytes   []byte
	URLType string // "" unless the value was a data URL
}

// Decode reads base64 (padded or not; line breaks ignored) or a data URL
// with base64 data (data:image/png;base64,...).
func Decode(s string) (Encoded, error) {
	var e Encoded
	if rest, ok := strings.CutPrefix(s, "data:"); ok {
		meta, data, found := strings.Cut(rest, ",")
		if !found {
			return e, errors.New("a data URL without a comma before its data")
		}
		params := strings.Split(meta, ";")
		if params[len(params)-1] != "base64" {
			return e, errors.New("a data URL that is not base64")
		}
		e.URLType = strings.ToLower(params[0])
		s = data
	}
	s = strings.NewReplacer("\r", "", "\n", "", " ", "").Replace(s)
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		var err2 error
		if b, err2 = base64.RawStdEncoding.DecodeString(s); err2 != nil {
			return e, fmt.Errorf("not valid base64: %v", err)
		}
	}
	e.Bytes = b
	return e, nil
}

// IsDataImage reports a string that is plainly an image data URL.
func IsDataImage(s string) bool {
	return strings.HasPrefix(s, "data:image/") && strings.Contains(s[:min(len(s), 100)], ";base64,")
}
