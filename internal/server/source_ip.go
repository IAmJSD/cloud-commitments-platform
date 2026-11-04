package server

import (
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/clientip"
)

// warnXFFIgnored makes the hops=0 warning fire once per process.
var warnXFFIgnored sync.Once

// requestSourceIP returns the client address the API rate limits key on.
//
// trustedHops is the number of proxies that append to X-Forwarded-For before
// the app. The entry that many places from the right is the client; the
// leftmost entries are client-controlled and trivially spoofable. With 0 hops,
// no header, too few entries or an entry that is not an IP address, the TCP
// peer is used. Either way a port is dropped: net/http's RemoteAddr is always
// "ip:port", and some proxies append "ip:port" to X-Forwarded-For, so keying
// on the raw value would give every connection its own bucket and the
// credential-endpoint limits would never bind (issue #104).
func requestSourceIP(r *http.Request, trustedHops int) string {
	// All header lines count as one list: a client can send its own line and
	// a proxy may append a separate one instead of concatenating.
	xff := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if xff != "" && trustedHops == 0 {
		warnXFFIgnored.Do(func() {
			log.Printf("WARN: X-Forwarded-For received but CUDLY_TRUSTED_PROXY_HOPS=0; using the peer address. Set it to the number of appending proxies in front of the app.")
		})
	}
	return clientip.Resolve(r.RemoteAddr, xff, trustedHops)
}
