package purchase

import (
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"testing"
)

// TestMain routes every proxy-aware HTTP client in this test binary (Go's
// default transport and the AWS SDK clients, which honor HTTP(S)_PROXY) into a
// local listener that counts and drops connections. If any non-loopback
// connection is attempted the run fails after the tests finish. Loopback
// traffic (httptest) bypasses the proxy and is unaffected. Not covered: code
// that dials directly (raw net.Dial, gRPC) or builds a transport with no proxy.
func TestMain(m *testing.M) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "no-network guard: listen:", err)
		os.Exit(1)
	}
	var attempts atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			attempts.Add(1)
			_ = c.Close()
		}
	}()
	proxy := "http://" + ln.Addr().String()
	for k, v := range map[string]string{
		"HTTP_PROXY": proxy, "HTTPS_PROXY": proxy, "http_proxy": proxy, "https_proxy": proxy,
		"NO_PROXY": "127.0.0.1,localhost", "no_proxy": "127.0.0.1,localhost",
		"AWS_EC2_METADATA_DISABLED": "true",
		"AWS_ACCESS_KEY_ID":         "test", "AWS_SECRET_ACCESS_KEY": "test",
	} {
		_ = os.Setenv(k, v)
	}
	code := m.Run()
	_ = ln.Close()
	if n := attempts.Load(); n > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: purchase tests attempted %d outbound network connection(s)\n", n)
		code = 1
	}
	os.Exit(code)
}
