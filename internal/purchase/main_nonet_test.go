package purchase

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"testing"
)

// loopbackOnly fails any HTTP request that leaves the machine, so a test that
// reaches a real cloud API fails loudly instead of silently spending money.
type loopbackOnly struct{ next http.RoundTripper }

func (l loopbackOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if ip := net.ParseIP(r.URL.Hostname()); r.URL.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("purchase tests must not make network calls: %s", r.URL.Redacted())
	}
	return l.next.RoundTrip(r)
}

func TestMain(m *testing.M) {
	http.DefaultTransport = loopbackOnly{next: http.DefaultTransport}
	// Point every AWS SDK client at a dead loopback port: an unstubbed call
	// fails at once and never reaches AWS.
	_ = os.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")
	_ = os.Setenv("AWS_ACCESS_KEY_ID", "test")
	_ = os.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	os.Exit(m.Run())
}
