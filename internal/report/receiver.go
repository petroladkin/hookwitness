package report

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/petroladkin/hookwitness/internal/emulate"
	"github.com/petroladkin/hookwitness/internal/store"
)

// receiverCounts sum up the receiver's side of the reported requests.
type receiverCounts struct {
	forwarded, senderLeft, notForwarded int
	answered                            map[string]int // 2xx, 4xx, 5xx, other
	failures                            map[string]int // by kind
}

func (c *receiverCounts) count(o emulate.Outcome) {
	if o.NotForwarded {
		c.notForwarded++
		return
	}
	n := o.Forward
	if n == nil || n.Pending {
		return
	}
	c.forwarded++
	if n.SenderLeftMs > 0 {
		c.senderLeft++
	}
	if n.ErrorKind != "" {
		if c.failures == nil {
			c.failures = map[string]int{}
		}
		c.failures[n.ErrorKind]++
		return
	}
	if c.answered == nil {
		c.answered = map[string]int{}
	}
	switch s := n.Status; {
	case s >= 200 && s < 300:
		c.answered[class2xx]++
	case s >= 400 && s < 500:
		c.answered[class4xx]++
	case s >= 500:
		c.answered[class5xx]++
	default:
		c.answered[classOther]++
	}
}

// line is the summary's receiver line: "forwarded 12 · answered 2xx 10,
// 5xx 1 · failed: timeout 1 · the sender left before the answer 1 · not
// forwarded (rules) 3"; "" when there is nothing to say.
func (c receiverCounts) line() string {
	if c.forwarded == 0 && c.notForwarded == 0 {
		return ""
	}
	parts := []string{"forwarded " + strconv.Itoa(c.forwarded)}
	var ans []string
	for _, k := range []string{class2xx, class4xx, class5xx, classOther} {
		if n := c.answered[k]; n > 0 {
			ans = append(ans, k+" "+strconv.Itoa(n))
		}
	}
	if len(ans) > 0 {
		parts = append(parts, "the receiver answered "+strings.Join(ans, ", "))
	}
	var fails []string
	for _, k := range []string{"dial", "tls", "timeout", "reset", "write", "answer", emulate.FailRefused, emulate.FailTooLarge,
		emulate.FailUpgrade, emulate.FailBusy} {
		if n := c.failures[k]; n > 0 {
			fails = append(fails, k+" "+strconv.Itoa(n))
		}
	}
	if len(fails) > 0 {
		parts = append(parts, "failed: "+strings.Join(fails, ", "))
	}
	if c.senderLeft > 0 {
		parts = append(parts, "the sender left before the answer "+strconv.Itoa(c.senderLeft))
	}
	if c.notForwarded > 0 {
		parts = append(parts, "not forwarded (rules) "+strconv.Itoa(c.notForwarded))
	}
	return strings.Join(parts, " · ")
}

// receiverLine is a request's receiver side in one line, "" when its
// endpoint does not forward.
func receiverLine(o emulate.Outcome) string {
	if o.NotForwarded {
		return "not forwarded: rule " + strconv.Itoa(o.Rule) + " took it"
	}
	n := o.Forward
	if n == nil {
		return ""
	}
	var s string
	switch {
	case n.Pending:
		return "being forwarded to " + site(n.Target)
	case n.ErrorKind != "":
		s = "no answer from " + site(n.Target) + ": " + emulate.FailureWords(n.ErrorKind) + " · after " + ms(n.Ms)
	default:
		s = strconv.Itoa(n.Status) + " from " + site(n.Target) + " in " + ms(n.Ms)
	}
	if o.Source == emulate.FromRule {
		s = "the receiver got it too: " + s
	}
	if n.SenderLeftMs > 0 {
		s += " · the sender left after " + ms(n.SenderLeftMs)
	}
	return s
}

// site is a receiver's scheme and host as written, without its path:
// reports are passed around, and some receivers (chat webhooks) carry their
// secret in the path.
func site(target string) string {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return "the receiver"
	}
	return u.Scheme + "://" + u.Host
}

// receiverBodyNote is the line about the receiver's answer body.
func receiverBodyNote(it Item) string {
	b := *it.ReceiverBody
	if b.Dropped {
		return size(b.Size) + " · not kept: " + store.AnswerDropped
	}
	s := bodyNote(b)
	if it.ReceiverCut {
		s += " · only the first 64 KiB kept"
	}
	return s
}

// receiverAnswer adds the receiver's answer lines and body to an item.
func (h *Head) receiverAnswer(it *Item, fw store.Forward) {
	if fw.Status == 0 {
		return
	}
	for _, hd := range fw.Headers {
		if h.mask[strings.ToLower(hd.Name)] {
			it.ReceiverHeaders = append(it.ReceiverHeaders, Header{hd.Name, maskValue(hd.Value), true})
			continue
		}
		it.ReceiverHeaders = append(it.ReceiverHeaders, Header{Name: hd.Name, Value: hd.Value})
	}
	b := viewBody(store.Request{Body: fw.Body, Size: int64(len(fw.Body)), BodyDropped: fw.BodyDropped}, h.bodyLimit)
	b.Size = fw.Size
	it.ReceiverBody = &b
	it.ReceiverCut = fw.Truncated
}
