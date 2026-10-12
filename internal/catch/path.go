package catch

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
)

// pathSegment is one segment of an endpoint path.
var pathSegment = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// MaxPathLen bounds an endpoint path.
const MaxPathLen = 512

// CheckPath validates an endpoint path under the admin prefix; the message
// is for the person (or client) that chose it, "" when the path is fine.
func CheckPath(prefix, p string) string {
	if !strings.HasPrefix(p, "/") {
		return `The path must start with "/", for example /stripe/webhooks.`
	}
	if len(p) > MaxPathLen {
		return fmt.Sprintf("The path is longer than %d characters.", MaxPathLen)
	}
	if p != "/" {
		segs := strings.Split(p[1:], "/")
		for i, seg := range segs {
			if seg == "" && i == len(segs)-1 {
				break // a trailing slash is part of the path
			}
			if seg == "" {
				return `The path has an empty segment ("//").`
			}
			if seg == "." || seg == ".." || !pathSegment.MatchString(seg) {
				return fmt.Sprintf("The segment %q is not allowed: use letters, digits and . _ ~ - (not . or .. alone).", seg)
			}
		}
	}
	if p == "/healthz" || p == prefix || strings.HasPrefix(p, prefix+"/") {
		return fmt.Sprintf("%s is reserved for hookwitness itself (health check and %s).", p, prefix)
	}
	return ""
}

// RandomPath is a path nobody will guess or collide with.
func RandomPath() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 12)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "/" + string(b)
}
