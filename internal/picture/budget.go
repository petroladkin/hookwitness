package picture

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"sync"
	"sync/atomic"
)

// DecodedBytes estimates the memory a full decode of b takes, from its
// header: the pixels at the bytes the decoder's colour model keeps for each,
// and the working buffers of the formats that need them. It errs high.
func DecodedBytes(b []byte) (int64, error) {
	info, cfg, err := header(b)
	if err != nil || !Decodable(info.Type) {
		return 0, err
	}
	return decodedBytes(b, info, cfg), nil
}

func decodedBytes(b []byte, info Info, cfg image.Config) int64 {
	px := int64(info.Width) * int64(info.Height)
	per := int64(bytesPerPixel(cfg.ColorModel))
	switch info.Type {
	case PNG:
		if len(b) > 28 && b[28] != 0 { // interlaced: each pass is decoded on its own
			per *= 2
		}
	case JPEG:
		if progressiveJPEG(b) { // coefficients of every block are kept: 4 B per component
			per += 12
		}
	case WebP:
		if bytes.Contains(b[:min(len(b), 64)], []byte("VP8L")) { // lossless: an ARGB buffer too
			per += 4
		}
	}
	return px*per + 64<<10
}

func bytesPerPixel(m color.Model) int {
	if _, ok := m.(color.Palette); ok {
		return 1
	}
	switch m {
	case color.GrayModel, color.AlphaModel:
		return 1
	case color.Gray16Model, color.Alpha16Model:
		return 2
	case color.YCbCrModel:
		return 3 // 4:4:4 at worst
	case color.RGBAModel, color.NRGBAModel, color.CMYKModel, color.NYCbCrAModel:
		return 4
	}
	return 8 // RGBA64, NRGBA64 and anything unknown
}

// progressiveJPEG finds a progressive frame header (SOF2) among the markers
// before the first scan.
func progressiveJPEG(b []byte) bool {
	for i := 2; i+3 < len(b); {
		if b[i] != 0xff {
			return false
		}
		m := b[i+1]
		switch {
		case m == 0xc2:
			return true
		case m == 0xda: // start of scan: no SOF2 before it
			return false
		case m == 0xff:
			i++
			continue
		}
		i += 2 + (int(b[i+2])<<8 | int(b[i+3]))
	}
	return false
}

// gate bounds the full decodes running at once in the whole process, so
// the memory they take is bounded however many requests arrive together.
var gate = make(chan struct{}, 2)

// decodeFull decodes every pixel (a variable for tests).
var decodeFull = func(b []byte) error {
	_, _, err := image.Decode(bytes.NewReader(b))
	return err
}

var decodes atomic.Int64

// Decodes is the number of full decodes so far (for tests and diagnostics).
func Decodes() int64 { return decodes.Load() }

func full(b []byte) error {
	gate <- struct{}{}
	defer func() { <-gate }()
	decodes.Add(1)
	return decodeFull(b)
}

// Budget decodes the images of one request: each at most once (the same
// bytes declared the same way give the same answer), and in all no more
// than its limit; an image past either bound is not decoded, and the reason
// is returned instead. It is not safe for concurrent use.
type Budget struct {
	limit, left int64
	seen        map[[sha256.Size]byte]verified
}

type verified struct {
	info       Info
	notChecked string
	err        error
}

// NewBudget allows limit bytes of decoding in all.
func NewBudget(limit int64) *Budget {
	return &Budget{limit: limit, left: limit, seen: map[[sha256.Size]byte]verified{}}
}

// Verify is Check and then, for a type decoded here, a full decode within
// the budget. notChecked says why the pixels were not decoded; err why b is
// not a valid image of the declared type.
func (bu *Budget) Verify(b []byte, declared string) (info Info, notChecked string, err error) {
	h := sha256.New()
	h.Write([]byte(declared + "\x00"))
	h.Write(b)
	var key [sha256.Size]byte
	h.Sum(key[:0])
	if v, ok := bu.seen[key]; ok {
		return v.info, v.notChecked, v.err
	}
	v := bu.verify(b, declared)
	bu.seen[key] = v
	return v.info, v.notChecked, v.err
}

func (bu *Budget) verify(b []byte, declared string) verified {
	info, cfg, err := header(b)
	if err == nil && declared != Any && declared != info.Type {
		err = fmt.Errorf("a %s, declared %s", short(info.Type), short(declared))
	}
	if err != nil || !Decodable(info.Type) {
		return verified{info: info, err: err}
	}
	need := decodedBytes(b, info, cfg)
	if need > MaxDecodedBytes {
		return overLimit(info, need)
	}
	if need > bu.left {
		return verified{info: info, notChecked: fmt.Sprintf("the request's images take more than %d MB to decode", bu.limit>>20)}
	}
	bu.left -= need
	return pixels(b, info)
}

func overLimit(info Info, need int64) verified {
	return verified{info: info, notChecked: fmt.Sprintf("decoding it takes %d MB, over the limit of %d MB for one image",
		need>>20, MaxDecodedBytes>>20)}
}

// pixels decodes b fully, through the gate.
func pixels(b []byte, info Info) verified {
	if err := full(b); err != nil {
		return verified{info: info, err: fmt.Errorf("a truncated or corrupt %s: %v", short(info.Type), err)}
	}
	return verified{info: info}
}

// viewed remembers the full check of images shown (a body that is an image,
// a data URL no schema declared), so showing a request again does not
// decode its images again. Cleared when full: a bound, not a policy.
var viewed = struct {
	sync.Mutex
	m map[[sha256.Size]byte]verified
}{m: map[[sha256.Size]byte]verified{}}

const viewedMax = 256

// verifyViewed is the full check of an image shown on a page or in a
// report: within the per-image limit, through the gate, remembered by digest.
func verifyViewed(b []byte, sum [sha256.Size]byte) verified {
	viewed.Lock()
	v, ok := viewed.m[sum]
	viewed.Unlock()
	if ok {
		return v
	}
	info, cfg, err := header(b)
	switch {
	case err != nil || !Decodable(info.Type):
		v = verified{info: info, err: err}
	case decodedBytes(b, info, cfg) > MaxDecodedBytes:
		v = overLimit(info, decodedBytes(b, info, cfg))
	default:
		v = pixels(b, info)
	}
	viewed.Lock()
	if len(viewed.m) >= viewedMax {
		clear(viewed.m)
	}
	viewed.m[sum] = v
	viewed.Unlock()
	return v
}
