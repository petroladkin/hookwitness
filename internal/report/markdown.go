package report

import (
	"bufio"
	"io"
	"strconv"
	"strings"

	"github.com/petroladkin/hookwitness/internal/auth"
)

// mdWriter writes GitHub/GitLab-flavoured Markdown: tables for the facts,
// fenced blocks for bodies with a fence longer than any backtick run inside.
type mdWriter struct {
	w     *bufio.Writer
	first bool
}

func newMarkdown(w io.Writer) *mdWriter { return &mdWriter{w: bufio.NewWriter(w), first: true} }

func (m *mdWriter) p(parts ...string) {
	for _, s := range parts {
		m.w.WriteString(s)
	}
}

func (m *mdWriter) head(h *Head) error {
	m.p("# Hookwitness report · ", mdText(h.Endpoint), "\n\n")
	m.p("| | |\n|---|---|\n")
	m.p("| Endpoint | ", mdCode(h.Endpoint), " |\n")
	m.p("| Time range | ", rangeLine(h), " |\n")
	m.p("| Selection | ", mdCell(h.Selection), " |\n")
	m.p("| Requests | ", strconv.Itoa(h.Count), " |\n")
	m.p("| Generated | ", stamp(h.Generated), " by Hookwitness ", mdCell(h.Version), " |\n")
	masked := "none"
	if len(h.Masked) > 0 {
		var names []string
		for _, n := range h.Masked {
			names = append(names, mdCode(n))
		}
		masked = strings.Join(names, ", ")
	}
	m.p("| Masked headers | ", masked, " |\n\n")

	m.p("## Summary\n\n**Answers**\n\n")
	var labels, seps, nums []string
	for _, c := range h.Outcomes {
		labels = append(labels, c.Label)
		seps = append(seps, "---:")
		nums = append(nums, strconv.Itoa(c.N))
	}
	m.p("| ", strings.Join(labels, " | "), " |\n|", strings.Join(seps, "|"), "|\n| ", strings.Join(nums, " | "), " |\n\n")

	m.p("**Verdicts**\n\n")
	if len(h.Checks) == 0 {
		m.p("No checks gave verdicts.\n\n")
	} else {
		m.p("| Check | pass | fail | not checked | not verifiable |\n|---|---:|---:|---:|---:|\n")
		for _, c := range h.Checks {
			m.p("| ", strconv.Itoa(c.Check), " · ", mdCell(c.Label), " | ", strconv.Itoa(c.Pass), " | ", strconv.Itoa(c.Fail),
				" | ", strconv.Itoa(c.NC), " | ", strconv.Itoa(c.NV), " |\n")
		}
		m.p("\n")
	}

	m.p("**Retries** (the same body delivered more than once)\n\n")
	if len(h.Retries) == 0 {
		m.p("None.\n\n")
	} else {
		m.p("| Deliveries | Gaps | Body SHA-256 |\n|---|---|---|\n")
		for _, r := range h.Retries {
			var nos, gaps []string
			for _, n := range r.Nos {
				nos = append(nos, "#"+strconv.FormatInt(n, 10))
			}
			for _, g := range r.Gaps {
				gaps = append(gaps, gap(g))
			}
			m.p("| ", strings.Join(nos, ", "), " | ", strings.Join(gaps, ", "), " | ", mdCode(r.SHA256[:16]+"…"), " |\n")
		}
		m.p("\n")
	}
	m.p("## Requests\n")
	if h.Count == 0 {
		m.p("\nNo requests in the selection.\n")
	}
	return nil
}

func (m *mdWriter) item(it Item) error {
	m.p("\n### #", strconv.FormatInt(it.No, 10), " · ", mdText(it.Method), " ", mdCode(target(it)), "\n\n")
	m.p("| | |\n|---|---|\n")
	m.p("| Received | ", stamp(it.At), " from ", mdCode(it.Remote), " · ", mdCell(it.Proto), " |\n")
	m.p("| Answer | ", mdCell(answerLine(it.Answer)), " |\n")
	if it.Delivery > 1 {
		m.p("| Delivery | ", ordinal(it.Delivery), " of this body (a retry) |\n")
	}
	m.p("\n")

	if len(it.Verdicts) > 0 {
		m.p("**Verdicts**\n\n")
		for _, v := range it.Verdicts {
			m.p("- ", mdText(verdictLine(v)), "\n")
			if v.Received != "" || v.Expected != "" {
				m.p("  - received ", mdCode(v.Received), ", expected ", mdCode(v.Expected), "\n")
			}
			for _, pr := range v.Problems {
				m.p("  - ", mdCode(pointer(pr)), " ", mdCode(pr.Keyword), " ", mdText(pr.Message), "\n")
			}
			if more := v.Total - len(v.Problems); more > 0 {
				m.p("  - and ", strconv.Itoa(more), " more\n")
			}
		}
		m.p("\n")
	}

	if len(it.Query) > 0 {
		m.p("**Query**\n\n| Name | Value |\n|---|---|\n")
		for _, q := range it.Query {
			m.p("| ", mdCode(q.Name), " | ", mdCode(q.Value), " |\n")
		}
		m.p("\n")
	}

	m.p("**Headers**\n\n")
	if len(it.Headers) == 0 {
		m.p("None.\n\n")
	} else {
		m.p("| Name | Value |\n|---|---|\n")
		for _, hd := range it.Headers {
			v := mdCode(hd.Value)
			if hd.Masked {
				v = mdCell(hd.Value)
			}
			m.p("| ", mdCode(hd.Name), " | ", v, " |\n")
		}
		m.p("\n")
	}

	if len(it.Images) > 0 {
		m.p("**Images**\n\n| Where | Image | SHA-256 |\n|---|---|---|\n")
		for _, img := range it.Images {
			sum := "—"
			if img.SHA256 != "" {
				sum = mdCode(img.SHA256[:16] + "…")
			}
			m.p("| ", mdCode(imageLabel(img.Found)), " | ", mdCell(imageNote(img.Found)), " | ", sum, " |\n")
		}
		m.p("\n")
	}

	m.p("**Body** · ", mdText(bodyNote(it.Body)), "\n")
	if it.Body.Kind == BodyJSON || it.Body.Kind == BodyText {
		lang := ""
		if it.Body.Kind == BodyJSON {
			lang = "json"
		}
		fence := strings.Repeat("`", max(3, longestRun(it.Body.Text, '`')+1))
		text := strings.TrimRight(it.Body.Text, "\n")
		m.p("\n", fence, lang, "\n", text, "\n", fence, "\n")
		if it.Body.Cut {
			m.p("\n*Cut at ", size(int64(it.Body.Shown)), ".*\n")
		}
	}
	return nil
}

func (m *mdWriter) end() error {
	return m.w.Flush()
}

// pointer names a problem's place; the whole body is "(body)".
func pointer(p auth.Problem) string {
	if p.Path == "" {
		return "(body)"
	}
	return p.Path
}

func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}

// longestRun is the longest run of c in s.
func longestRun(s string, c byte) int {
	best, n := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			n++
			best = max(best, n)
		} else {
			n = 0
		}
	}
	return best
}

// mdCode is s as an inline code span that survives backticks, pipes and
// line breaks inside a table cell.
func mdCode(s string) string {
	if s == "" {
		return "—"
	}
	s = oneLine(s)
	s = strings.ReplaceAll(s, "|", `\|`)
	ticks := strings.Repeat("`", longestRun(s, '`')+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return ticks + s + ticks
}

// mdCell is plain text safe in a table cell.
func mdCell(s string) string {
	if s == "" {
		return "—"
	}
	return strings.ReplaceAll(mdText(oneLine(s)), "|", `\|`)
}

// mdText escapes the characters that would start Markdown or HTML.
func mdText(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '`', '*', '_', '[', ']', '<', '>', '~':
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func oneLine(s string) string {
	return lineBreaks.Replace(s)
}

var lineBreaks = strings.NewReplacer("\r\n", " ⏎ ", "\n", " ⏎ ", "\r", " ⏎ ")
