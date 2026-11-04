package email

import "strings"

// SameAddress reports whether two email addresses are equal after trimming
// surrounding whitespace, ignoring ASCII letter case only. Every approver and
// requester comparison must go through it so they always agree.
func SameAddress(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		// ASCII-only: matching must never be broader than exact-or-ASCII-case.
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// ContainsAddress reports whether addrs holds an address equal to target
// under SameAddress.
func ContainsAddress(addrs []string, target string) bool {
	for _, addr := range addrs {
		if SameAddress(addr, target) {
			return true
		}
	}
	return false
}
