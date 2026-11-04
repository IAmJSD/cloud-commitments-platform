// Package clientip normalises client addresses for rate-limit keys and
// resolves the client address behind trusted proxies.
package clientip

import (
	"net/netip"
	"strings"
)

// invalidKey is the single shared bucket for unparsable addresses, so a
// malformed value never buys a fresh rate-limit budget.
const invalidKey = "invalid"

// ipv6KeyBits is the prefix a single IPv6 subscriber commonly controls.
const ipv6KeyBits = 64

// parseAddr accepts a bare address, "ip:port", "[v6]:port" or "[v6]", with an
// optional zone. IPv4-mapped IPv6 is unmapped and zones are dropped.
func parseAddr(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap().WithZone(""), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap().WithZone(""), true
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		if a, err := netip.ParseAddr(s[1 : len(s)-1]); err == nil {
			return a.Unmap().WithZone(""), true
		}
	}
	return netip.Addr{}, false
}

// RateLimitKey returns the per-IP rate-limit key for a source address: IPv4
// unchanged, IPv6 masked to its /64, anything unparsable the shared "invalid"
// key. /56 and /48 rotation by one client still works; only /64 is collapsed.
func RateLimitKey(sourceIP string) string {
	a, ok := parseAddr(sourceIP)
	if !ok {
		return invalidKey
	}
	if a.Is4() {
		return a.String()
	}
	p, err := a.Prefix(ipv6KeyBits)
	if err != nil {
		return invalidKey
	}
	return p.String()
}

// Resolve returns the client address for a request that crossed trustedHops
// appending proxies. With n X-Forwarded-For entries it uses entry n-hops;
// with hops <= 0, fewer entries than hops, or an unparsable or empty entry it
// falls back to the peer address, so unparsable text is never used as a key.
// IPv6 stays full-width; use RateLimitKey for the /64 key.
func Resolve(remoteAddr, xff string, trustedHops int) string {
	peer := remoteAddr
	if a, ok := parseAddr(remoteAddr); ok {
		peer = a.String()
	}
	if trustedHops <= 0 || strings.TrimSpace(xff) == "" {
		return peer
	}
	// Scan from the right, at most trustedHops entries, so the work does not
	// grow with the header length.
	end := len(xff)
	for i := 1; ; i++ {
		comma := strings.LastIndexByte(xff[:end], ',')
		if i == trustedHops {
			if a, ok := parseAddr(xff[comma+1 : end]); ok {
				return a.String()
			}
			return peer
		}
		if comma < 0 {
			return peer
		}
		end = comma
	}
}
