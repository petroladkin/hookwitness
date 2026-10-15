package picture

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// zeroPNG writes a PNG of w×h zero pixels row by row, so that a test can
// make a huge image without holding its pixels: depth 8 or 16, colour type
// 0 (gray), 2 (RGB) or 6 (RGBA).
func zeroPNG(t testing.TB, w, h int, depth, colorType byte) []byte {
	t.Helper()
	channels := map[byte]int{0: 1, 2: 3, 6: 4}[colorType]
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
	ihdr[8], ihdr[9] = depth, colorType
	chunk("IHDR", ihdr)
	var z bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&z, zlib.BestSpeed)
	row := make([]byte, 1+w*channels*int(depth)/8) // filter byte 0, then zeros
	for range h {
		zw.Write(row)
	}
	zw.Close()
	chunk("IDAT", z.Bytes())
	chunk("IEND", nil)
	return out.Bytes()
}

func encoded(t testing.TB, img image.Image, kind string) []byte {
	t.Helper()
	var b bytes.Buffer
	var err error
	switch kind {
	case "png":
		err = png.Encode(&b, img)
	case "jpeg":
		err = jpeg.Encode(&b, img, nil)
	case "gif":
		err = gif.Encode(&b, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The estimate is never below what a decode allocates, and not wildly above.
func TestDecodedBytesBoundsTheDecode(t *testing.T) {
	r := image.Rect(0, 0, 600, 400)
	pal := image.NewPaletted(r, color.Palette{color.Black, color.White, color.Transparent})
	cases := map[string][]byte{
		"gray8 PNG":    encoded(t, image.NewGray(r), "png"),
		"gray16 PNG":   encoded(t, image.NewGray16(r), "png"),
		"RGBA PNG":     encoded(t, image.NewNRGBA(r), "png"),
		"RGBA64 PNG":   encoded(t, image.NewNRGBA64(r), "png"),
		"paletted PNG": encoded(t, pal, "png"),
		"JPEG":         encoded(t, image.NewRGBA(r), "jpeg"),
		"GIF":          encoded(t, pal, "gif"),
		"zero RGBA16":  zeroPNG(t, 600, 400, 16, 6),
	}
	for name, b := range cases {
		est, err := DecodedBytes(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		if err := decodeFull(b); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		runtime.ReadMemStats(&after)
		got := int64(after.TotalAlloc - before.TotalAlloc)
		if est < got || est > 3*got+(1<<20) {
			t.Errorf("%s: estimated %d, the decode allocated %d", name, est, got)
		}
	}
	// An interlaced PNG is decoded pass by pass: its estimate doubles.
	b := zeroPNG(t, 600, 400, 8, 6)
	flat, _ := DecodedBytes(b)
	b[28] = 1 // interlace method, then the IHDR checksum again
	binary.BigEndian.PutUint32(b[29:], crc32.ChecksumIEEE(b[12:29]))
	if laced, _ := DecodedBytes(b); laced-64<<10 != 2*(flat-64<<10) {
		t.Errorf("interlaced %d, flat %d", laced, flat)
	}
}

// A small file that decodes to a lot is not decoded: 16-bit RGBA of
// 4100×4100 is 134 MB of pixels in a few hundred kilobytes.
func TestOverTheLimitIsNotDecoded(t *testing.T) {
	b := zeroPNG(t, 4100, 4100, 16, 6)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	info, notChecked, err := NewBudget(RequestBudget).Verify(b, PNG)
	runtime.ReadMemStats(&after)
	if err != nil || info.Width != 4100 || !strings.Contains(notChecked, "over the limit of 128 MB for one image") {
		t.Fatalf("%+v %q %v", info, notChecked, err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 8<<20 {
		t.Fatalf("allocated %d MB", n>>20)
	}
	if _, err := Verify(b, PNG); err == nil || !strings.HasPrefix(err.Error(), "not checked: ") {
		t.Fatalf("Verify: %v", err)
	}
}

// A budget decodes until it is spent; the same image again is answered from
// memory, decoded once.
func TestBudget(t *testing.T) {
	var decodes atomic.Int64
	old := decodeFull
	decodeFull = func(b []byte) error { decodes.Add(1); return old(b) }
	defer func() { decodeFull = old }()
	a, b, c := zeroPNG(t, 500, 500, 16, 0), zeroPNG(t, 501, 500, 16, 0), zeroPNG(t, 502, 500, 16, 0)
	need, _ := DecodedBytes(a)
	bu := NewBudget(2*need + need/2)
	for i, img := range [][]byte{a, a, b, c} {
		_, notChecked, err := bu.Verify(img, Any)
		if err != nil {
			t.Fatal(err)
		}
		if want := i == 3; (notChecked != "") != want {
			t.Errorf("image %d: not checked %q", i, notChecked)
		}
	}
	if n := decodes.Load(); n != 2 {
		t.Fatalf("%d decodes, want 2 (a once, b; c over the budget)", n)
	}
}

// At most two full decodes run at once, however many are asked for.
func TestGate(t *testing.T) {
	var now, most atomic.Int64
	release := make(chan struct{})
	old := decodeFull
	decodeFull = func([]byte) error {
		n := now.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		<-release
		now.Add(-1)
		return nil
	}
	defer func() { decodeFull = old }()
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() { defer wg.Done(); full(nil) }()
	}
	for now.Load() < 2 { // wait until the gate is full
		runtime.Gosched()
	}
	for range 6 {
		release <- struct{}{}
	}
	wg.Wait()
	if most.Load() != 2 {
		t.Fatalf("%d decodes at once", most.Load())
	}
}
