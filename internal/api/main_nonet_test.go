package api

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// netGuard is the no-network guard TestMain installs for this package (#855).
var netGuard *noNetworkGuard

// afterAllTests runs after every test in the package has finished. The
// integration build registers the shared PostgreSQL container's teardown here
// (main_integration_test.go).
var afterAllTests []func()

// TestMain keeps this package's tests hermetic (#855). Before any test runs it:
//
//   - strips every AWS_* variable and points the shared config and
//     credentials files at the null device, so no test can pick up a
//     developer's or runner's real AWS identity, profile or region;
//   - routes every proxy-aware HTTP client (Go's default transport, the AWS
//     SDK including its IMDS client, the Azure and GCP SDKs) into a local
//     listener that records and drops the connection;
//   - replaces net.DefaultResolver with one whose name server is a local
//     pipe, so a direct dial to a host name outside the hosts file fails and
//     the looked-up name is recorded.
//
// Loopback traffic (httptest servers, local Postgres) bypasses the proxy and
// is unaffected. If anything was blocked, the run fails after the tests finish
// and names the blocked targets; each one is also printed as it happens, so
// with -v it lands next to the test that made it. Not covered: dials to IP
// literals by code that builds a transport with no proxy, and resolvers other
// than net.DefaultResolver.
func TestMain(m *testing.M) {
	g, err := startNoNetworkGuard()
	if err != nil {
		fmt.Fprintln(os.Stderr, "no-network guard:", err)
		os.Exit(1)
	}
	netGuard = g
	code := m.Run()
	for _, f := range afterAllTests {
		f()
	}
	if blocked := g.stop(); len(blocked) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: internal/api tests attempted %d outbound network call(s); stub the client instead (#855):\n  %s\n",
			len(blocked), strings.Join(blocked, "\n  "))
		code = 1
	}
	os.Exit(code)
}

type noNetworkGuard struct {
	ln      net.Listener
	mu      sync.Mutex
	blocked []string
}

func startNoNetworkGuard() (*noNetworkGuard, error) {
	if err := isolateAWSEnv(); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	g := &noNetworkGuard{ln: ln}
	proxy := "http://" + ln.Addr().String()
	for k, v := range map[string]string{
		"HTTP_PROXY": proxy, "HTTPS_PROXY": proxy, "http_proxy": proxy, "https_proxy": proxy,
		"NO_PROXY": "127.0.0.1,localhost,::1", "no_proxy": "127.0.0.1,localhost,::1",
	} {
		if err := os.Setenv(k, v); err != nil {
			_ = ln.Close()
			return nil, err
		}
	}
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			client, server := net.Pipe()
			go g.recordDNSQuery(server)
			return client, nil
		},
	}
	go g.serve()
	return g, nil
}

// isolateAWSEnv clears the ambient AWS SDK configuration so tests resolve the
// same (empty) identity locally as in CI. IMDS stays enabled on purpose: a
// test that falls through to it is then caught by the proxy, not hidden.
func isolateAWSEnv() error {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "AWS_") {
			if err := os.Unsetenv(k); err != nil {
				return err
			}
		}
	}
	for _, k := range []string{"AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE"} {
		if err := os.Setenv(k, os.DevNull); err != nil {
			return err
		}
	}
	return nil
}

// serve accepts proxied connections, records the request line (CONNECT host
// for HTTPS, the absolute URL for plain HTTP) and drops the connection.
func (g *noNetworkGuard) serve() {
	for {
		c, err := g.ln.Accept()
		if err != nil {
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, _ := bufio.NewReader(c).ReadString('\n')
		g.record(strings.TrimSpace(line))
		_ = c.Close()
	}
}

// recordDNSQuery reads the query the resolver writes to its (fake) name
// server connection, records the looked-up name and drops the connection, so
// the lookup fails without leaving the machine.
func (g *noNetworkGuard) recordDNSQuery(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1024)
	n, _ := c.Read(buf)
	// A pipe is not a net.PacketConn, so the resolver uses DNS-over-TCP
	// framing: a 2-byte length prefix before the message.
	msg := buf[:n]
	if len(msg) >= 2 {
		msg = msg[2:]
	}
	g.record("DNS lookup " + dnsQuestionName(msg))
}

// dnsQuestionName extracts the first question's name from a DNS query.
func dnsQuestionName(msg []byte) string {
	const headerLen = 12
	var labels []string
	for i := headerLen; i < len(msg) && msg[i] != 0; {
		n := int(msg[i])
		if n > 63 || i+1+n > len(msg) {
			return "(unparsed query)"
		}
		labels = append(labels, string(msg[i+1:i+1+n]))
		i += 1 + n
	}
	if len(labels) == 0 {
		return "(unparsed query)"
	}
	return strings.Join(labels, ".")
}

func (g *noNetworkGuard) record(what string) {
	fmt.Fprintf(os.Stderr, "no-network guard: blocked %s\n", what)
	g.mu.Lock()
	g.blocked = append(g.blocked, what)
	g.mu.Unlock()
}

// takeMatching removes and returns the blocked entries containing substr, so
// the guard's own self-test does not fail the run.
func (g *noNetworkGuard) takeMatching(substr string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var taken, kept []string
	for _, b := range g.blocked {
		if strings.Contains(b, substr) {
			taken = append(taken, b)
		} else {
			kept = append(kept, b)
		}
	}
	g.blocked = kept
	return taken
}

func (g *noNetworkGuard) stop() []string {
	_ = g.ln.Close()
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.blocked...)
}

// TestNoNetworkGuard_BlocksOutboundCalls pins the guard itself: an HTTPS call
// through the default client and a DNS lookup through net.DefaultResolver must
// both be stopped locally and recorded. The entries are taken back out so this
// self-test does not fail the run.
func TestNoNetworkGuard_BlocksOutboundCalls(t *testing.T) {
	const host = "nonet-guard-selftest.invalid"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "an outbound HTTPS call must not succeed under the guard")
	assert.Equal(t, []string{"CONNECT " + host + ":443 HTTP/1.1"}, netGuard.takeMatching("CONNECT "+host),
		"the HTTPS call must reach the guard's proxy and be recorded")

	_, err = net.DefaultResolver.LookupHost(ctx, host)
	require.Error(t, err, "a DNS lookup must not succeed under the guard")
	assert.NotEmpty(t, netGuard.takeMatching("DNS lookup "+host),
		"the DNS lookup must reach the guard's resolver and be recorded")
}
