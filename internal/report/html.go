package report

import (
	"bufio"
	"encoding/base64"
	"html/template"
	"io"
	"io/fs"
	"strings"
)

// htmlWriter writes one self-contained HTML file: inline CSS in the
// recorder look (light, dark by prefers-color-scheme, print rules), the
// fonts inlined as data URIs, an inline SVG strip, and a CSP that forbids
// every request, so opening it fetches nothing.
type htmlWriter struct {
	w     *bufio.Writer
	fonts fs.FS
}

func newHTML(w io.Writer, fonts fs.FS) *htmlWriter {
	return &htmlWriter{w: bufio.NewWriter(w), fonts: fonts}
}

func (x *htmlWriter) head(h *Head) error {
	return htmlTemplates.ExecuteTemplate(x.w, "head", struct {
		*Head
		CSS   template.CSS
		Strip strip
	}{h, template.CSS(fontFaces(x.fonts) + reportCSS), newStrip(h)})
}

func (x *htmlWriter) item(it Item) error {
	return htmlTemplates.ExecuteTemplate(x.w, "item", it)
}

func (x *htmlWriter) end() error {
	if err := htmlTemplates.ExecuteTemplate(x.w, "end", nil); err != nil {
		return err
	}
	return x.w.Flush()
}

// webFonts are the UI's woff2 files a report embeds: family, weight, file,
// unicode-range.
var webFonts = []struct{ family, weight, file, unicodes string }{
	{"Saira Stencil One", "400", "saira-stencil-one/saira-stencil-one-latin-400-normal.woff2", rangeLatin},
	{"Atkinson Hyperlegible", "400", "atkinson-hyperlegible/atkinson-hyperlegible-latin-400-normal.woff2", rangeLatin},
	{"Atkinson Hyperlegible", "400", "atkinson-hyperlegible/atkinson-hyperlegible-latin-ext-400-normal.woff2", rangeLatinExt},
	{"Atkinson Hyperlegible", "700", "atkinson-hyperlegible/atkinson-hyperlegible-latin-700-normal.woff2", rangeLatin},
	{"Atkinson Hyperlegible", "700", "atkinson-hyperlegible/atkinson-hyperlegible-latin-ext-700-normal.woff2", rangeLatinExt},
	{"Martian Mono", "400", "martian-mono/martian-mono-latin-400-normal.woff2", rangeLatin},
	{"Martian Mono", "400", "martian-mono/martian-mono-latin-ext-400-normal.woff2", rangeLatinExt},
	{"Martian Mono", "400", "martian-mono/martian-mono-cyrillic-400-normal.woff2", rangeCyrillic},
	{"Martian Mono", "600", "martian-mono/martian-mono-latin-600-normal.woff2", rangeLatin},
	{"Martian Mono", "600", "martian-mono/martian-mono-latin-ext-600-normal.woff2", rangeLatinExt},
	{"Martian Mono", "600", "martian-mono/martian-mono-cyrillic-600-normal.woff2", rangeCyrillic},
}

const (
	rangeLatin    = "U+0000-00FF,U+0131,U+0152-0153,U+02BB-02BC,U+02C6,U+02DA,U+02DC,U+0304,U+0308,U+0329,U+2000-206F,U+20AC,U+2122,U+2191,U+2193,U+2212,U+2215,U+FEFF,U+FFFD"
	rangeLatinExt = "U+0100-02BA,U+02BD-02C5,U+02C7-02CC,U+02CE-02D7,U+02DD-02FF,U+0304,U+0308,U+0329,U+1D00-1DBF,U+1E00-1E9F,U+1EF2-1EFF,U+2020,U+20A0-20AB,U+20AD-20C0,U+2113,U+2C60-2C7F,U+A720-A7FF"
	rangeCyrillic = "U+0301,U+0400-045F,U+0490-0491,U+04B0-04B1,U+2116"
)

// fontFaces is the @font-face rules with the files inlined; a file that is
// missing is left out and the fallback fonts take its place.
func fontFaces(fonts fs.FS) string {
	if fonts == nil {
		return ""
	}
	var sb strings.Builder
	for _, f := range webFonts {
		b, err := fs.ReadFile(fonts, "vendor/"+f.file)
		if err != nil {
			continue
		}
		sb.WriteString(`@font-face{font-family:"` + f.family + `";font-style:normal;font-weight:` + f.weight +
			`;src:url(data:font/woff2;base64,` + base64.StdEncoding.EncodeToString(b) + `) format("woff2");unicode-range:` + f.unicodes + "}\n")
	}
	return sb.String()
}

var htmlTemplates = template.Must(template.New("report").Funcs(template.FuncMap{
	"stamp":    stamp,
	"num":      num,
	"gap":      gap,
	"span":     rangeLine,
	"alarm":    func(stamp string) bool { return !strings.HasPrefix(stamp, "2") },
	"note":     bodyNote,
	"answer":   answerLine,
	"target":   target,
	"ordinal":  ordinal,
	"pointer":  pointer,
	"short":    func(s string) string { return s[:min(16, len(s))] + "…" },
	"sub":      func(a, b int) int { return a - b },
	"lower":    strings.ToLower,
	"imgLabel": imageLabel,
	"imgNote":  imageNote,
	"dataURI": func(img ItemImage) template.URL {
		return template.URL("data:" + img.Type + ";base64," + base64.StdEncoding.EncodeToString(img.Data))
	},
	"size":   func(n int) string { return size(int64(n)) },
	"top":    func() float64 { return stripTop },
	"base":   func() float64 { return stripBase },
	"w":      func() float64 { return stripW },
	"h":      func() float64 { return stripH },
	"isJSON": func(b Body) bool { return b.Kind == BodyJSON },
	"isText": func(b Body) bool { return b.Kind == BodyText },
	"tokenClass": func(class string, bad bool) string {
		if bad {
			return strings.TrimSpace(class + " bad")
		}
		return class
	},
}).Parse(htmlSource))

const htmlSource = `{{define "head" -}}
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; font-src data:; img-src data:">
<meta name="generator" content="Hookwitness {{.Version}}">
<title>Hookwitness report · {{.Endpoint}}</title>
<style>
{{.CSS}}</style>
</head>
<body>
<div class="wrap">
<header class="mast">
<div class="brand">Hookwitness</div>
<h1>Report · <code>{{.Endpoint}}</code></h1>
</header>
<dl class="facts">
<div><dt>Time range</dt><dd>{{span .Head}}</dd></div>
<div><dt>Selection</dt><dd>{{.Selection}}</dd></div>
<div><dt>Requests</dt><dd>{{.Count}}</dd></div>
<div><dt>Generated</dt><dd>{{stamp .Generated}} by Hookwitness {{.Version}}</dd></div>
<div><dt>Masked headers</dt><dd>{{if .Masked}}{{range $i, $n := .Masked}}{{if $i}}, {{end}}<code>{{$n}}</code>{{end}}{{else}}none{{end}}</dd></div>
</dl>
{{with .Strip}}<figure class="strip">
<svg viewBox="0 0 {{num w}} {{num h}}" preserveAspectRatio="none" role="img" aria-label="Requests over the reported range">
<rect class="paper" x="0" y="0" width="{{num w}}" height="{{num h}}"/>
{{range .Grid}}<line class="{{if .Major}}grid-major{{else}}grid{{end}}" x1="{{num .X}}" y1="{{num (top)}}" x2="{{num .X}}" y2="{{num (base)}}"/>
{{end}}{{range .Rows}}<line class="grid" x1="0" y1="{{num .}}" x2="{{num w}}" y2="{{num .}}"/>
{{end}}<line class="baseline" x1="0" y1="{{num (base)}}" x2="{{num w}}" y2="{{num (base)}}"/>
{{range .Ticks}}<text class="tick" x="{{num .X}}" y="{{num (h)}}" dy="-8">{{.Label}}</text>
{{end}}{{range .Brackets}}<path class="bracket" d="M{{num .A}} {{num .Y}}v-6H{{num .B}}v6"/><text class="gap" x="{{num .A}}" y="{{num .Y}}" dy="-10">{{.Label}}</text>
{{end}}{{range .Marks}}{{if .Failed}}<rect class="alarm" x="{{num .X}}" y="{{num (base)}}" width="8" height="3" transform="translate(-4 3)"/>
{{end}}{{if eq .Kind "timeout"}}<line class="stem bad dashed" x1="{{num .X}}" y1="{{num (base)}}" x2="{{num .X}}" y2="{{num (top)}}"/><circle class="ring bad" cx="{{num .X}}" cy="{{num (top)}}" r="4"/>
{{else if eq .Kind "drop"}}<line class="stem bad" x1="{{num .X}}" y1="{{num (base)}}" x2="{{num .X}}" y2="{{num .Tip}}"/><path class="stem bad" d="M{{num .X}} {{num .Tip}}m-5 -5l10 10m0 -10l-10 10"/>
{{else}}<line class="stem {{.Kind}}" x1="{{num .X}}" y1="{{num (base)}}" x2="{{num .X}}" y2="{{num .Tip}}"/><circle class="dot {{.Kind}}" cx="{{num .X}}" cy="{{num .Tip}}" r="3"/>
{{end}}{{end}}</svg>
<figcaption>Height = body size · red: not 2xx · dashed: timeout · ×: dropped · red bar under the line: a failed check · same body bracketed · times in UTC</figcaption>
</figure>
{{end}}<section class="summary">
<h2>Summary</h2>
<div class="tables">
<table class="counts">
<caption>Answers</caption>
<tr>{{range .Outcomes}}<th scope="col">{{.Label}}</th>{{end}}</tr>
<tr>{{range .Outcomes}}<td class="{{if and .N (ne .Label "2xx")}}hot{{end}}">{{.N}}</td>{{end}}</tr>
</table>
<table class="checks">
<caption>Verdicts</caption>
{{if .Checks}}<tr><th scope="col">Check</th><th scope="col">pass</th><th scope="col">fail</th><th scope="col">not checked</th><th scope="col">not verifiable</th></tr>
{{range .Checks}}<tr><th scope="row">{{.Check}} · {{.Label}}</th><td>{{.Pass}}</td><td class="{{if .Fail}}hot{{end}}">{{.Fail}}</td><td>{{.NC}}</td><td>{{.NV}}</td></tr>
{{end}}{{else}}<tr><td class="none">No checks gave verdicts.</td></tr>
{{end}}</table>
</div>
<table class="retries">
<caption>Retries · the same body delivered more than once</caption>
{{if .Retries}}<tr><th scope="col">Deliveries</th><th scope="col">Gaps</th><th scope="col">Body SHA-256</th></tr>
{{range .Retries}}<tr><td>{{range $i, $n := .Nos}}{{if $i}}, {{end}}<a href="#r{{$n}}">#{{$n}}</a>{{end}}</td><td>{{range $i, $g := .Gaps}}{{if $i}}, {{end}}{{gap $g}}{{end}}</td><td><code title="{{.SHA256}}">{{short .SHA256}}</code></td></tr>
{{end}}{{else}}<tr><td class="none">None.</td></tr>
{{end}}</table>
</section>
<h2 class="requests-title">Requests</h2>
{{if not .Count}}<p class="none">No requests in the selection.</p>
{{end}}{{end}}

{{define "item" -}}
<article class="exhibit" id="r{{.No}}">
<header>
<span class="no">#{{.No}}</span>
<span class="line"><b>{{.Method}}</b> <code>{{target .}}</code></span>
<span class="stamp{{if alarm .Answer.Stamp}} alarm{{end}}">{{.Answer.Stamp}}</span>
</header>
<dl class="facts">
<div><dt>Received</dt><dd>{{stamp .At}} from <code>{{.Remote}}</code> · {{.Proto}}</dd></div>
<div><dt>Answer</dt><dd>{{answer .Answer}}</dd></div>
{{if gt .Delivery 1}}<div><dt>Delivery</dt><dd>{{ordinal .Delivery}} of this body (a retry)</dd></div>
{{end}}</dl>
{{if .Verdicts}}<section class="verdicts">
<h3>Verdicts</h3>
<ul>
{{range .Verdicts}}<li class="v-{{lower (printf "%s" .Result)}}"><b>{{.Label}}: {{.Result}}</b>{{if .Reason}} — {{.Reason}}{{end}}{{if or .Received .Expected}}<div class="sig">received <code>{{.Received}}</code> · expected <code>{{.Expected}}</code></div>{{end}}{{if .Problems}}
<table class="problems">{{range .Problems}}<tr><th>{{pointer .}}</th><td><code>{{.Keyword}}</code></td><td>{{.Message}}</td></tr>{{end}}</table>{{if gt .Total (len .Problems)}}<div class="more">and {{sub .Total (len .Problems)}} more</div>{{end}}{{end}}</li>
{{end}}</ul>
</section>
{{end}}{{if .Query}}<section>
<h3>Query</h3>
<table class="kv">{{range .Query}}<tr><th><code>{{.Name}}</code></th><td><code>{{.Value}}</code></td></tr>{{end}}</table>
</section>
{{end}}<section>
<h3>Headers</h3>
{{if .Headers}}<table class="kv">{{range .Headers}}<tr><th><code>{{.Name}}</code></th><td>{{if .Masked}}<span class="masked">{{.Value}}</span>{{else}}<code>{{.Value}}</code>{{end}}</td></tr>{{end}}</table>
{{else}}<p class="none">None.</p>
{{end}}</section>
{{if .Images}}<section>
<h3>Images</h3>
<div class="images">{{range .Images}}<figure{{if .Error}} class="bad"{{end}}>{{if .Data}}<img src="{{dataURI .}}" alt="The image at {{imgLabel .Found}}">{{end}}<figcaption><code>{{imgLabel .Found}}</code> {{imgNote .Found}}</figcaption></figure>{{end}}</div>
</section>
{{end}}<section>
<h3>Body <span class="note">{{note .Body}}</span></h3>
{{if isJSON .Body}}<pre class="body json">{{range .Body.Tokens}}{{if or .Class .Bad}}<span class="{{tokenClass .Class .Bad}}">{{.Text}}</span>{{else}}{{.Text}}{{end}}{{if .Elided}}<span class="elided">{{.Elided}}</span>{{end}}{{end}}</pre>
{{else if isText .Body}}<pre class="body">{{.Body.Text}}</pre>
{{end}}{{if .Body.Cut}}<p class="note">Cut at {{size .Body.Shown}}.</p>
{{end}}</section>
</article>
{{end}}

{{define "end" -}}
<footer class="foot">Written by Hookwitness · times are UTC</footer>
</div>
</body>
</html>
{{end}}`

// reportCSS is the recorder look of the UI, trimmed to a document.
const reportCSS = `:root{
  --paper:#E6ECE7;--paper-2:#DCE4DE;--grid:#B9CDBF;--grid-major:#93AF9C;
  --ink:#24308C;--ink-soft:#4B56A6;--alarm:#C2331F;--text:#17201B;--muted:#58695F;--rule:#C3D2C8;
  --f-stamp:"Saira Stencil One","Arial Narrow",Impact,sans-serif;
  --f-body:"Atkinson Hyperlegible",system-ui,-apple-system,"Segoe UI",sans-serif;
  --f-data:"Martian Mono",ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
  color-scheme:light dark;
}
@media (prefers-color-scheme:dark){:root{
  --paper:#0F1613;--paper-2:#141D19;--grid:#1E2C25;--grid-major:#2C4136;
  --ink:#9AA8FF;--ink-soft:#7884D6;--alarm:#FF6E58;--text:#DAE4DD;--muted:#86998E;--rule:#22322A}}
*{box-sizing:border-box}
body{margin:0;background:var(--paper);color:var(--text);font:400 15px/1.5 var(--f-body);padding-inline:16px;padding-block:0 32px}
.wrap{max-width:1100px;margin:0 auto}
.wrap>*{min-width:0}
a{color:var(--ink)}
code,pre{font-family:var(--f-data)}
code{font-size:.86em}
h1{font:600 18px/1.3 var(--f-data);margin:0;overflow-wrap:anywhere}
h1 code{font-size:1em}
h2{font:400 15px/1 var(--f-stamp);letter-spacing:.08em;text-transform:uppercase;color:var(--ink);margin:28px 0 12px}
h3{font:700 12px/1.2 var(--f-body);letter-spacing:.06em;text-transform:uppercase;color:var(--muted);margin:16px 0 6px}
.mast{display:flex;flex-wrap:wrap;align-items:baseline;gap:8px 18px;padding-block:18px 10px;border-bottom:2px solid var(--ink)}
.brand{font:400 22px/1 var(--f-stamp);letter-spacing:.04em;color:var(--ink);text-transform:uppercase}
.facts{display:grid;grid-template-columns:repeat(auto-fit,minmax(min(100%,230px),1fr));gap:8px 24px;margin:14px 0}
.facts div{min-width:0}
.facts dt{font:700 11px/1.2 var(--f-body);letter-spacing:.06em;text-transform:uppercase;color:var(--muted)}
.facts dd{margin:2px 0 0;font:400 13px/1.45 var(--f-data);overflow-wrap:anywhere}
.strip{margin:8px 0 0;border:1px solid var(--rule);background:var(--paper)}
.strip svg{display:block;width:100%;height:132px}
.strip figcaption{font-size:12px;color:var(--muted);padding:6px 10px;border-top:1px solid var(--rule)}
.paper{fill:var(--paper)}
.grid{stroke:var(--grid);stroke-width:1;vector-effect:non-scaling-stroke}
.grid-major{stroke:var(--grid-major);stroke-width:1;vector-effect:non-scaling-stroke}
.baseline{stroke:var(--ink);stroke-opacity:.35;vector-effect:non-scaling-stroke}
.tick,.gap{font:400 10px var(--f-data);fill:var(--muted);text-anchor:middle}
.gap{fill:var(--ink);font-weight:600;text-anchor:start}
.bracket{fill:none;stroke:var(--ink-soft);stroke-dasharray:3 3;vector-effect:non-scaling-stroke}
.stem{stroke:var(--ink);stroke-width:2;fill:none;vector-effect:non-scaling-stroke}
.stem.bad{stroke:var(--alarm)}
.dashed{stroke-dasharray:3 3}
.dot{fill:var(--ink)}.dot.bad{fill:var(--alarm)}
.ring{fill:none;stroke:var(--alarm);stroke-width:2;vector-effect:non-scaling-stroke}
.alarm{fill:var(--alarm)}
.summary .tables{display:flex;flex-wrap:wrap;gap:16px 32px;align-items:flex-start}
table{border-collapse:collapse;font-size:13px}
caption{text-align:left;font:700 11px/1.2 var(--f-body);letter-spacing:.06em;text-transform:uppercase;color:var(--muted);padding-bottom:6px}
th,td{text-align:left;padding:4px 12px 4px 0;border-bottom:1px solid var(--rule);vertical-align:top}
.counts td,.checks td{font:600 15px/1.2 var(--f-data);font-variant-numeric:tabular-nums}
.counts th{font-weight:400;color:var(--muted)}
.hot{color:var(--alarm)}
.retries{margin-top:16px;width:100%}
.summary caption{white-space:nowrap}
.checks td.none{font:inherit;color:var(--muted)}
.none{color:var(--muted)}
.requests-title{margin-top:36px}
.exhibit{background:var(--paper-2);border-left:3px solid var(--ink);padding:12px 16px 14px;margin:0 0 18px;break-inside:auto}
.exhibit>header{display:flex;flex-wrap:wrap;align-items:baseline;gap:6px 14px;break-after:avoid}
.exhibit .no{font:600 15px/1 var(--f-data);color:var(--ink)}
.exhibit .line{font:400 14px/1.4 var(--f-data);overflow-wrap:anywhere;min-width:0}
.exhibit .stamp{margin-left:auto;font:400 13px/1 var(--f-stamp);letter-spacing:.08em;text-transform:uppercase;color:var(--ink);border:2px solid currentColor;padding:4px 7px 3px;transform:rotate(-2deg)}
.exhibit .stamp.alarm{color:var(--alarm)}
.kv{width:100%;table-layout:fixed}
.kv th{width:32%;font-weight:400}
.kv td,.kv th{overflow-wrap:anywhere}
.masked{font:400 12px var(--f-data);color:var(--muted)}
.verdicts ul{list-style:none;margin:0;padding:0;display:grid;gap:6px}
.verdicts li{padding-left:10px;border-left:3px solid var(--ink)}
.verdicts li.v-fail{border-color:var(--alarm)}
.verdicts li.v-fail b{color:var(--alarm)}
.verdicts li.v-n\/v,.verdicts li.v-n\/c{border-color:var(--muted)}
.sig{font-size:12px;color:var(--muted);overflow-wrap:anywhere}
.problems{margin-top:4px}
.problems th{font:400 12px var(--f-data)}
.more{font-size:12px;color:var(--muted)}
.note{font:400 12px/1.4 var(--f-data);text-transform:none;letter-spacing:0;color:var(--muted);overflow-wrap:anywhere}
pre.body{margin:0;padding:10px 12px;background:var(--paper);border:1px solid var(--rule);font-size:12px;line-height:1.5;white-space:pre-wrap;overflow-wrap:anywhere}
.json .k{color:var(--ink)}.json .s{color:var(--text)}.json .n{color:var(--ink-soft)}
.json .elided{color:var(--muted);font-style:italic}
.json .bad{color:var(--alarm);text-decoration:underline wavy var(--alarm);text-underline-offset:3px}
.images{display:grid;grid-template-columns:repeat(auto-fill,minmax(min(100%,220px),1fr));gap:12px}
.images figure{margin:0;border-top:2px solid var(--ink);padding-top:6px;min-width:0}
.images figure.bad{border-top-color:var(--alarm)}
.images img{display:block;max-width:100%;max-height:240px;margin:0 0 6px;background:var(--paper);border:1px solid var(--rule)}
.images figcaption{font:400 12px/1.4 var(--f-data);color:var(--muted);overflow-wrap:anywhere}
.images figcaption code{color:var(--text)}
.foot{margin-top:32px;font-size:12px;color:var(--muted)}
@media print{
  :root{--paper:#fff;--paper-2:#fff;--grid:#B9CDBF;--grid-major:#93AF9C;--ink:#24308C;--ink-soft:#4B56A6;
    --alarm:#C2331F;--text:#000;--muted:#58695F;--rule:#C3D2C8;color-scheme:light}
  body{font-size:11pt;padding:0}
  .exhibit{border:1px solid var(--rule);border-left:3px solid var(--ink)}
  .exhibit>header,.exhibit .facts,.verdicts{break-inside:avoid}
  .strip{break-inside:avoid}
  a{color:inherit;text-decoration:none}
}
@page{size:A4;margin:15mm}
`
