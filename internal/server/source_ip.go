package server

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// requestSourceIP returns the client address the API rate limits key on.
//
// It takes the rightmost X-Forwarded-For entry, which is set by the nearest
// trusted proxy (ALB, Cloud Run ingress, etc.); the leftmost entries are
// client-controlled and trivially spoofable. Without that header it uses the
// TCP peer. Either way a port is dropped: net/http's RemoteAddr is always
// "ip:port", and some proxies append "ip:port" to X-Forwarded-For, so keying
// on the raw value would give every connection its own bucket and the
// credential-endpoint limits would never bind (issue #104). A rightmost entry
// that is not an IP address is not trusted as a key; the peer is used instead.
func requestSourceIP(r *http.Request) string {
	peer := stripPort(r.RemoteAddr)
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return peer
	}
	parts := strings.Split(xff, ",")
	last := stripPort(strings.TrimSpace(parts[len(parts)-1]))
	if _, err := netip.ParseAddr(last); err != nil {
		return peer
	}
	return last
}

// stripPort returns addr without a ":port" suffix (and without the brackets
// around an IPv6 host). An address with no port, including a bare IPv6
// address, is returned unchanged.
func stripPort(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}
