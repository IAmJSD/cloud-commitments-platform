package mocks

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// fatalT is a mock.TestingT whose FailNow stops the calling goroutine, like a
// real *testing.T, and which keeps the formatted failure messages so a test can
// check what was reported.
type fatalT struct {
	mu   sync.Mutex
	msgs []string
}

func (f *fatalT) Logf(string, ...interface{}) {}

func (f *fatalT) Errorf(format string, args ...interface{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, fmt.Sprintf(format, args...))
}

func (f *fatalT) FailNow() { runtime.Goexit() }

func (f *fatalT) messages() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.msgs, "\n")
}

var _ mock.TestingT = (*fatalT)(nil)

// callOutcome is what happened when a mock method ran in its own goroutine.
type callOutcome struct {
	returned bool        // the call returned normally
	panicked interface{} // the recovered panic value, if it panicked
}

// runIsolated runs fn in its own goroutine so a FailNow (runtime.Goexit) or a
// panic inside a mock ends that goroutine rather than the test.
func runIsolated(fn func()) callOutcome {
	var out callOutcome
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { out.panicked = recover() }()
		fn()
		out.returned = true
	}()
	<-done
	return out
}

// wrongType is a .Return value no mocked method returns.
type wrongType struct{}

// TestMockConfigStore_WrongReturnTypeFailsRegisteredTest pins the #353 fix on
// one method of each dispatch style: GetGlobalConfig serves a default unless an
// expectation is registered, GetServiceConfig always dispatches.
func TestMockConfigStore_WrongReturnTypeFailsRegisteredTest(t *testing.T) {
	for _, tc := range []struct {
		method string
		call   func(*MockConfigStore)
	}{
		{"GetGlobalConfig", func(m *MockConfigStore) { _, _ = m.GetGlobalConfig(context.Background()) }},
		{"GetServiceConfig", func(m *MockConfigStore) { _, _ = m.GetServiceConfig(context.Background(), "aws", "ec2") }},
	} {
		t.Run(tc.method, func(t *testing.T) {
			ft := &fatalT{}
			m := &MockConfigStore{}
			m.Test(ft)
			m.On(tc.method, mock.Anything, mock.Anything, mock.Anything).Maybe().Return(wrongType{}, nil)
			m.On(tc.method, mock.Anything).Maybe().Return(wrongType{}, nil)

			out := runIsolated(func() { tc.call(m) })

			assert.Nil(t, out.panicked, "a mistyped .Return must fail the registered test, not panic")
			assert.False(t, out.returned, "the failure must stop the test goroutine, as t.Fatal does")
			assert.Contains(t, ft.messages(), "mock: "+tc.method+" returned mocks.wrongType")
		})
	}
}

// TestMock_WrongReturnTypePanicsWithoutRegisteredTest documents the fallback:
// with no test registered there is nowhere to report to, so the mock still
// panics, now with a message that names the method.
func TestMock_WrongReturnTypePanicsWithoutRegisteredTest(t *testing.T) {
	m := &MockConfigStore{}
	m.On("GetGlobalConfig", mock.Anything).Return(wrongType{}, nil)

	out := runIsolated(func() { _, _ = m.GetGlobalConfig(context.Background()) })

	require.NotNil(t, out.panicked)
	assert.Contains(t, fmt.Sprint(out.panicked), "mock: GetGlobalConfig returned mocks.wrongType")
}

// TestMock_TestStillRegistersWithTestify checks the shadowing Test method still
// hands t to testify: an unexpected call fails the test instead of panicking.
func TestMock_TestStillRegistersWithTestify(t *testing.T) {
	ft := &fatalT{}
	m := &MockAuthStore{}
	m.Test(ft)

	out := runIsolated(func() { _, _ = m.GetUserByID(context.Background(), "user-1") })

	assert.Nil(t, out.panicked, "testify must fail the registered test on an unexpected call")
	assert.False(t, out.returned)
	assert.Contains(t, ft.messages(), "GetUserByID")
}

// TestMocks_EveryWrongReturnTypeSiteReportsItsOwnMethod drives every mock
// method that calls wrongReturnType with a mistyped .Return and checks the
// failure reaches the registered test under that method's name. It catches a
// site whose method-name argument was copied from a neighboring method, and a mock type
// whose Test method does not wire up its reporter.
//
// The sites are found by parsing the package source, so methods added later
// are covered without editing this test.
func TestMocks_EveryWrongReturnTypeSiteReportsItsOwnMethod(t *testing.T) {
	sites := wrongReturnTypeSites(t)
	require.GreaterOrEqual(t, len(sites), 80,
		"found only %d wrongReturnType call sites; the source scan has broken", len(sites))

	newMock := map[string]func() (mockUnderTest, interface{}){
		"MockConfigStore": func() (mockUnderTest, interface{}) {
			m := &MockConfigStore{}
			return m, m
		},
		"MockAuthStore": func() (mockUnderTest, interface{}) {
			m := &MockAuthStore{}
			return m, m
		},
		"MockSecretsManagerClient": func() (mockUnderTest, interface{}) {
			m := &MockSecretsManagerClient{}
			return m, m
		},
		"MockSESClient": func() (mockUnderTest, interface{}) {
			m := &MockSESClient{}
			return m, m
		},
		"MockSNSClient": func() (mockUnderTest, interface{}) {
			m := &MockSNSClient{}
			return m, m
		},
	}

	for _, s := range sites {
		t.Run(s.recvType+"."+s.method, func(t *testing.T) {
			ctor, ok := newMock[s.recvType]
			require.True(t, ok, "no constructor for mock type %s; add it to this test", s.recvType)
			m, recv := ctor()
			ft := &fatalT{}
			m.Test(ft)
			matchers := make([]interface{}, s.calledArity)
			for i := range matchers {
				matchers[i] = mock.Anything
			}
			m.On(s.method, matchers...).Return(wrongType{}, nil, nil, nil)

			method := reflect.ValueOf(recv).MethodByName(s.method)
			require.True(t, method.IsValid(), "method %s not found", s.method)
			in := zeroArgs(method.Type())

			out := runIsolated(func() { method.Call(in) })

			assert.Nil(t, out.panicked, "%s panicked instead of failing the registered test", s.method)
			assert.False(t, out.returned, "%s returned instead of failing the registered test", s.method)
			assert.Contains(t, ft.messages(), "mock: "+s.method+" returned mocks.wrongType",
				"%s must report the mistyped .Return under its own name", s.method)
		})
	}
}

// mockUnderTest is the part of a testify mock this file drives.
type mockUnderTest interface {
	Test(mock.TestingT)
	On(methodName string, arguments ...interface{}) *mock.Call
}

// zeroArgs builds an argument list for a method: a live context, a non-nil
// pointer for each pointer parameter (so a method that reads a field before
// dispatching does not crash first), and zero values otherwise.
func zeroArgs(ft reflect.Type) []reflect.Value {
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	n := ft.NumIn()
	if ft.IsVariadic() {
		n--
	}
	in := make([]reflect.Value, 0, n)
	for i := 0; i < n; i++ {
		p := ft.In(i)
		switch {
		case p == ctxType:
			in = append(in, reflect.ValueOf(context.Background()))
		case p.Kind() == reflect.Ptr:
			in = append(in, reflect.New(p.Elem()))
		default:
			in = append(in, reflect.Zero(p))
		}
	}
	return in
}

// wrongReturnTypeSite is one mock method that reports a mistyped .Return.
type wrongReturnTypeSite struct {
	recvType    string
	method      string
	calledArity int // number of arguments the method passes to m.Called
}

// wrongReturnTypeSites parses the package's non-test files for methods whose
// body calls <recv>.reporter.wrongReturnType.
func wrongReturnTypeSites(t *testing.T) []wrongReturnTypeSite {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var sites []wrongReturnTypeSite
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			if site, ok := siteFromDecl(decl); ok {
				sites = append(sites, site)
			}
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		return sites[i].recvType+"."+sites[i].method < sites[j].recvType+"."+sites[j].method
	})
	return sites
}

func siteFromDecl(decl ast.Decl) (wrongReturnTypeSite, bool) {
	fd, ok := decl.(*ast.FuncDecl)
	if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 || fd.Body == nil {
		return wrongReturnTypeSite{}, false
	}
	star, ok := fd.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return wrongReturnTypeSite{}, false
	}
	recvIdent, ok := star.X.(*ast.Ident)
	if !ok {
		return wrongReturnTypeSite{}, false
	}
	site := wrongReturnTypeSite{recvType: recvIdent.Name, method: fd.Name.Name, calledArity: -1}
	reports := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "wrongReturnType":
			reports = true
		case "Called":
			site.calledArity = len(call.Args)
		}
		return true
	})
	return site, reports && site.calledArity >= 0
}
