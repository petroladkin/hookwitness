package web

import (
	"encoding/json"

	"github.com/petroladkin/hookwitness/internal/payload"
)

// previewLimit bounds how much of a body is rendered on the page; the
// download always has all of it.
const previewLimit = 256 << 10

// hexLimit bounds the hex preview of a binary body.
const hexLimit = 512

// The body helpers live in internal/payload, shared with the reports.
type (
	token      = payload.Token
	queryParam = payload.QueryParam
)

var (
	prettyJSON = payload.Pretty
	isText     = payload.IsText
	hexDump    = payload.HexDump
	parseQuery = payload.ParseQuery
	markBad    = payload.MarkBad
	markImages = payload.MarkImages
)

// bodyKind is how a body is shown.
type bodyKind int

const (
	bodyEmpty bodyKind = iota
	bodyJSON
	bodyText
	bodyBinary
)

// bodyView is a body prepared for the page.
type bodyView struct {
	Kind      bodyKind
	JSON      []token // bodyJSON
	Text      string  // bodyText; the raw view of bodyJSON
	Hex       string  // bodyBinary
	Truncated bool    // only the first previewLimit (hexLimit) bytes are shown
}

func viewBody(b []byte) bodyView {
	switch {
	case len(b) == 0:
		return bodyView{Kind: bodyEmpty}
	case len(b) <= previewLimit && json.Valid(b):
		if toks, err := prettyJSON(b); err == nil {
			return bodyView{Kind: bodyJSON, JSON: toks, Text: string(b)}
		}
	}
	shown, cut := b, false
	if len(shown) > previewLimit {
		shown, cut = shown[:previewLimit], true
	}
	if isText(shown) {
		return bodyView{Kind: bodyText, Text: string(shown), Truncated: cut}
	}
	n := min(len(b), hexLimit)
	return bodyView{Kind: bodyBinary, Hex: hexDump(b[:n]), Truncated: len(b) > n}
}
