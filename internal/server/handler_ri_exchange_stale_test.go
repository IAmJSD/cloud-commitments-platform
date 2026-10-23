package server

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/testutil"
)

// Unit coverage for #692: the scheduled reshape run raises an operator alarm
// for processing RI exchanges stranded by a crashed run. The store is a mock;
// the resolve path is covered against real PostgreSQL in internal/config.

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs routes the package logger to a buffer for the test. Not parallel:
// the logger is process-global.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	out := &lockedBuffer{}
	prev := logging.SetOutput(out)
	t.Cleanup(func() { logging.SetOutput(prev) })
	return out
}

func errorLines(logs string) []string {
	var lines []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "[ERROR]") {
			lines = append(lines, line)
		}
	}
	return lines
}

// A stranded processing row must raise an ERROR naming the row, its hold and
// how to resolve it, even while automation is disabled, and must not be
// settled or released by the scheduled run.
func TestHandleRIExchangeReshape_AlarmsOnStrandedProcessingExchange(t *testing.T) {
	logs := captureLogs(t)
	var gotOlderThan time.Duration
	strandedAt := time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)
	store := &mockConfigStoreForExchangeStale{
		mockConfigStoreForExchange: mockConfigStoreForExchange{globalConfig: &config.GlobalConfig{RIExchangeEnabled: false}},
		staleFunc: func(_ context.Context, olderThan time.Duration) ([]config.RIExchangeRecord, error) {
			gotOlderThan = olderThan
			return []config.RIExchangeRecord{{
				ID: "6f1c2a8e-0d4b-4c1e-9a3f-692000000001", Region: "us-east-1", Status: "processing",
				SourceRIIDs: []string{"ri-stranded"}, PaymentDue: "1000.000000", UpdatedAt: strandedAt,
			}}, nil
		},
	}
	app := &Application{Config: store}

	result, err := app.handleRIExchangeReshape(testutil.TestContext(t))
	testutil.AssertNoError(t, err)
	if result != nil {
		t.Fatal("disabled automation must still return a nil result")
	}
	if gotOlderThan != riExchangeStaleAlarmAfter {
		t.Fatalf("stale query threshold = %s, want %s", gotOlderThan, riExchangeStaleAlarmAfter)
	}
	lines := errorLines(logs.String())
	if len(lines) != 1 {
		t.Fatalf("want exactly one ERROR line for the stranded row, got %d:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{
		"RI exchange reconciliation needed", "record_id=6f1c2a8e-0d4b-4c1e-9a3f-692000000001", "region=us-east-1",
		"source_ri_ids=[ri-stranded]", "held_usd=1000.000000", "processing_since=2026-10-01T09:30:00Z",
		"POST /api/ri-exchange/resolve/6f1c2a8e-0d4b-4c1e-9a3f-692000000001",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("alarm line missing %q:\n%s", want, lines[0])
		}
	}
}

func TestAlarmStaleRIExchanges_QueryFailureIsAnError(t *testing.T) {
	logs := captureLogs(t)
	store := &mockConfigStoreForExchangeStale{
		staleFunc: func(context.Context, time.Duration) ([]config.RIExchangeRecord, error) {
			return nil, errors.New("db down")
		},
	}
	app := &Application{Config: store}
	if n := app.alarmStaleRIExchanges(testutil.TestContext(t)); n != 0 {
		t.Fatalf("reported %d rows on a failed query", n)
	}
	lines := errorLines(logs.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "failed to list stale processing exchanges: db down") {
		t.Fatalf("a failed stale check must itself raise an ERROR, got:\n%s", logs.String())
	}
}

func TestAlarmStaleRIExchanges_QuietWithoutStaleRows(t *testing.T) {
	logs := captureLogs(t)
	app := &Application{Config: &mockConfigStoreForExchangeStale{}}
	if n := app.alarmStaleRIExchanges(testutil.TestContext(t)); n != 0 {
		t.Fatalf("reported %d rows with none stale", n)
	}
	if lines := errorLines(logs.String()); len(lines) != 0 {
		t.Fatalf("no stale rows must log no ERROR, got:\n%s", logs.String())
	}
}
