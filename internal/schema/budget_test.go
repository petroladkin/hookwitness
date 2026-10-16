package schema

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"runtime"
	"strings"
	"testing"

	"github.com/petroladkin/hookwitness/internal/picture"
)

// gray16 writes a 16-bit gray PNG of w×h zero pixels row by row (a huge one
// without holding its pixels); rgba makes it 16-bit RGBA instead.
func gray16(w, h int, rgba bool) string {
	ct, ch := byte(0), 1
	if rgba {
		ct, ch = 6, 4
	}
	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(kind string, data []byte) {
		binary.Write(&out, binary.BigEndian, uint32(len(data)))
		out.WriteString(kind)
		out.Write(data)
		binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(kind), data...)))
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8], ihdr[9] = 16, ct
	chunk("IHDR", ihdr)
	var z bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&z, zlib.BestSpeed)
	row := make([]byte, 1+w*ch*2)
	for range h {
		zw.Write(row)
	}
	zw.Close()
	chunk("IDAT", z.Bytes())
	chunk("IEND", nil)
	return base64.StdEncoding.EncodeToString(out.Bytes())
}

const imageSchema = `{"type":"object","additionalProperties":{"type":"string","contentEncoding":"base64","contentMediaType":"image/png"}}`

// The review's bomb: images that decode to far more than they weigh. None
// is decoded; each is recorded as not checked, and validating costs little.
func TestImageBomb(t *testing.T) {
	big := gray16(4100, 4100, true) // 134 MB of pixels
	body := fmt.Sprintf(`{"a":%q,"b":%q,"c":%q,"d":%q}`, big, big, big, big)
	set, err := Compile([]Doc{{Name: "S", Text: imageSchema}})
	if err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	res := set.Validate("S", []byte(body))
	runtime.ReadMemStats(&after)
	if n := after.TotalAlloc - before.TotalAlloc; n > 32<<20 {
		t.Fatalf("validating allocated %d MB", n>>20)
	}
	if !res.OK || len(res.Images) != 4 || Unchecked(res.Images) != 4 || !strings.Contains(WhyUnchecked(res.Images), "over the limit") {
		t.Fatalf("%+v", res)
	}
}

// The budget goes to the images in path order, the same every time; the
// rest are recorded as not checked.
func TestImageBudgetInPathOrder(t *testing.T) {
	old := imageBudget
	defer func() { imageBudget = old }()
	img := []string{gray16(300, 300, false), gray16(301, 300, false), gray16(302, 300, false)}
	need, err := picture.DecodedBytes(mustB64(img[0]))
	if err != nil {
		t.Fatal(err)
	}
	imageBudget = 2*need + need/2
	set, _ := Compile([]Doc{{Name: "S", Text: imageSchema}})
	body := fmt.Sprintf(`{"c":%q,"a":%q,"b":%q}`, img[2], img[0], img[1])
	for range 5 {
		res := set.Validate("S", []byte(body))
		var got []string
		for _, im := range res.Images {
			got = append(got, fmt.Sprintf("%s:%v", im.Path, im.NotChecked != ""))
		}
		if strings.Join(got, " ") != "/a:false /b:false /c:true" || !res.OK {
			t.Fatalf("%v %+v", got, res)
		}
	}
}

// Each image is decoded once per validation: the same image at two paths,
// and a schema whose anyOf branches each declare it.
func TestImagesDecodedOnce(t *testing.T) {
	img := gray16(64, 64, false)
	other := gray16(65, 64, false)
	set, err := Compile([]Doc{{Name: "S", Text: `{"type":"object","properties":{
		"x":{"anyOf":[{"type":"string","contentEncoding":"base64","contentMediaType":"image/png"},
		              {"type":"string","contentEncoding":"base64","contentMediaType":"image/*"}]},
		"y":{"type":"string","contentEncoding":"base64","contentMediaType":"image/png"},
		"z":{"type":"string","contentEncoding":"base64","contentMediaType":"image/png"}}}`}})
	if err != nil {
		t.Fatal(err)
	}
	before := picture.Decodes()
	res := set.Validate("S", []byte(fmt.Sprintf(`{"x":%q,"y":%q,"z":%q}`, img, img, other)))
	if !res.OK || len(res.Images) != 3 {
		t.Fatalf("%+v", res)
	}
	if n := picture.Decodes() - before; n != 2 {
		t.Fatalf("%d full decodes, want 2 (the image once, the other once)", n)
	}
}

func mustB64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}
