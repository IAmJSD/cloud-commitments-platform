package server

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The source IP keys the per-IP rate limits on login, setup-admin and
// password reset. A port-bearing key would put every TCP connection in a
// fresh bucket, so those limits would never bind (issue #104).
func TestRequestSourceIP(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{name: "no XFF: IPv4 peer port is dropped", remoteAddr: "203.0.113.7:51234", want: "203.0.113.7"},
		{name: "no XFF: same client on another connection shares the key", remoteAddr: "203.0.113.7:40001", want: "203.0.113.7"},
		{name: "no XFF: IPv6 peer port and brackets are dropped", remoteAddr: "[2001:db8::1]:443", want: "2001:db8::1"},
		{name: "no XFF: peer without a port is kept", remoteAddr: "203.0.113.7", want: "203.0.113.7"},
		{name: "XFF: rightmost entry wins", remoteAddr: "10.0.0.5:9000", xff: "198.51.100.1, 203.0.113.9", want: "203.0.113.9"},
		{name: "XFF: rightmost entry with a port is reduced to the address", remoteAddr: "10.0.0.5:9000", xff: "198.51.100.1, 203.0.113.9:6123", want: "203.0.113.9"},
		{name: "XFF: bare IPv6 rightmost entry is kept", remoteAddr: "10.0.0.5:9000", xff: "2001:db8::2", want: "2001:db8::2"},
		{name: "XFF: bracketed IPv6 rightmost entry with a port is reduced", remoteAddr: "10.0.0.5:9000", xff: "[2001:db8::2]:6123", want: "2001:db8::2"},
		{name: "XFF: unparseable rightmost entry falls back to the peer address", remoteAddr: "10.0.0.5:9000", xff: "198.51.100.1, not-an-ip", want: "10.0.0.5"},
		{name: "XFF: empty rightmost entry falls back to the peer address", remoteAddr: "10.0.0.5:9000", xff: "198.51.100.1, ", want: "10.0.0.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), "POST", "/api/auth/login", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			assert.Equal(t, tc.want, requestSourceIP(req))
		})
	}
}

// httpToLambdaRequest must take its SourceIP from requestSourceIP. This reads
// the source rather than calling httpToLambdaRequest so it does not depend on
// that function's signature (other in-flight changes alter it).
func TestHTTPToLambdaRequest_UsesRequestSourceIP(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "http.go", nil, 0)
	require.NoError(t, err)
	var found, calls bool
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "httpToLambdaRequest" {
			return true
		}
		found = true
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			if c, ok := m.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "requestSourceIP" {
					calls = true
				}
			}
			return true
		})
		return false
	})
	require.True(t, found, "httpToLambdaRequest not found in http.go")
	assert.True(t, calls, "httpToLambdaRequest must derive SourceIP via requestSourceIP")
}
