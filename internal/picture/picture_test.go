package picture

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// webp1x1 is a lossless 1×1 WebP.
const webp1x1 = "UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA=="

func sample(t *testing.T, kind string, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{200, 50, 30, 255})
	var buf bytes.Buffer
	var err error
	switch kind {
	case PNG:
		err = png.Encode(&buf, img)
	case JPEG:
		err = jpeg.Encode(&buf, img, nil)
	case GIF:
		err = gif.Encode(&buf, img, nil)
	case WebP:
		b, _ := base64.StdEncoding.DecodeString(webp1x1)
		return b
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestVerify(t *testing.T) {
	for _, kind := range []string{PNG, JPEG, GIF, WebP} {
		b := sample(t, kind, 3, 2)
		info, err := Verify(b, kind)
		if err != nil || info.Type != kind {
			t.Fatalf("%s: %+v %v", kind, info, err)
		}
		if kind != WebP && (info.Width != 3 || info.Height != 2) {
			t.Fatalf("%s: %dx%d", kind, info.Width, info.Height)
		}
		if _, err := Verify(b, Any); err != nil {
			t.Fatalf("%s as any image: %v", kind, err)
		}
	}
	jpg := sample(t, JPEG, 4, 4)
	for name, tc := range map[string]struct {
		b        []byte
		declared string
		want     string
	}{
		"a JPEG declared PNG": {jpg, PNG, "a JPEG, declared PNG"},
		"truncated PNG":       {sample(t, PNG, 40, 40)[:60], PNG, "truncated or corrupt PNG"},
		"text":                {[]byte("hello"), Any, "not a PNG, JPEG, GIF, WebP, HEIC or AVIF image"},
		"SVG":                 {[]byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`), Any, "an SVG document"},
		"broken header":       {[]byte("\x89PNG\r\n\x1a\nxx"), PNG, "a broken PNG header"},
	} {
		if _, err := Verify(tc.b, tc.declared); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestTooManyPixels(t *testing.T) {
	// A PNG header claiming 10000×5000 pixels: refused before decoding.
	var buf bytes.Buffer
	png.Encode(&buf, image.NewGray(image.Rect(0, 0, 1, 1)))
	b := buf.Bytes()
	b[16], b[17], b[18], b[19] = 0, 0, 0x27, 0x10 // width 10000
	b[20], b[21], b[22], b[23] = 0, 0, 0x13, 0x88 // height 5000
	binary.BigEndian.PutUint32(b[29:33], crc32.ChecksumIEEE(b[12:29]))
	if _, err := Inspect(b); err == nil || !strings.Contains(err.Error(), "10000×5000 pixels, over the limit of 40 megapixels") {
		t.Fatalf("%v", err)
	}
}

func TestHEICIsRecognisedNotDecoded(t *testing.T) {
	b := append([]byte("\x00\x00\x00\x18ftypheic"), make([]byte, 16)...)
	info, err := Verify(b, HEIC)
	if err != nil || info.Type != HEIC || info.Width != 0 || Decodable(info.Type) {
		t.Fatalf("%+v %v", info, err)
	}
	if _, err := Verify(b, PNG); err == nil {
		t.Fatal("a HEIC declared PNG passed")
	}
}

func TestDecode(t *testing.T) {
	raw := sample(t, PNG, 1, 1)
	std := base64.StdEncoding.EncodeToString(raw)
	for name, s := range map[string]string{
		"padded":      std,
		"unpadded":    strings.TrimRight(std, "="),
		"line breaks": std[:10] + "\r\n" + std[10:],
		"data URL":    "data:image/png;base64," + std,
	} {
		e, err := Decode(s)
		if err != nil || !bytes.Equal(e.Bytes, raw) {
			t.Fatalf("%s: %v", name, err)
		}
		if (name == "data URL") != (e.URLType == PNG) {
			t.Fatalf("%s: url type %q", name, e.URLType)
		}
	}
	for s, want := range map[string]string{
		"not*base64!":           "not valid base64",
		"data:image/png,abc":    "not base64",
		"data:image/png;base64": "without a comma",
	} {
		if _, err := Decode(s); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", s, err, want)
		}
	}
	if !IsDataImage("data:image/jpeg;base64,/9j/") || IsDataImage("data:text/plain;base64,aGk=") || IsDataImage("image/png") {
		t.Fatal("IsDataImage")
	}
}
