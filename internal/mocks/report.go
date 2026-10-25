package mocks

import (
	"sync"

	"github.com/stretchr/testify/mock"
)

// testReporter routes a mock's own test-authoring errors to the test that owns
// the mock, so they fail that test instead of crashing the test binary.
//
// The error it handles today is a .Return(...) value whose type does not match
// what the mocked method returns (wrongReturnType, in stores.go). That used to
// be a bare panic, which ends the whole test binary: every remaining test in
// the package goes unrun, TestMain teardown (a shared Postgres container, for
// example) is skipped, and the output is a stack trace rather than a failure
// filed under the test's name.
//
// The test registers itself with the mock's Test method, the same hook testify
// already uses to fail an unexpected call through the test instead of
// panicking. A mock with no registered test keeps the old panic, because there
// is nothing else to report to.
type testReporter struct {
	mu sync.Mutex
	t  mock.TestingT
}

func (r *testReporter) set(t mock.TestingT) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.t = t
}

// fail fails the registered test with msg (Errorf plus FailNow, as testify
// does for an unexpected call), or panics with msg when no test is registered.
func (r *testReporter) fail(msg string) {
	r.mu.Lock()
	t := r.t
	r.mu.Unlock()
	if t == nil {
		panic(msg)
	}
	if h, ok := t.(interface{ Helper() }); ok {
		h.Helper()
	}
	fatalf(t, "%s", msg)
}

// Test registers t with the mock: an unexpected call (testify's own check) and
// a mistyped .Return value (see testReporter) then fail t instead of
// panicking. It shadows the promoted mock.Mock.Test and calls it.
func (m *MockConfigStore) Test(t mock.TestingT) {
	m.Mock.Test(t)
	m.reporter.set(t)
}

// Test registers t with the mock. See MockConfigStore.Test.
func (m *MockAuthStore) Test(t mock.TestingT) {
	m.Mock.Test(t)
	m.reporter.set(t)
}

// Test registers t with the mock. See MockConfigStore.Test.
func (m *MockSecretsManagerClient) Test(t mock.TestingT) {
	m.Mock.Test(t)
	m.reporter.set(t)
}

// Test registers t with the mock. See MockConfigStore.Test.
func (m *MockSESClient) Test(t mock.TestingT) {
	m.Mock.Test(t)
	m.reporter.set(t)
}

// Test registers t with the mock. See MockConfigStore.Test.
func (m *MockSNSClient) Test(t mock.TestingT) {
	m.Mock.Test(t)
	m.reporter.set(t)
}
