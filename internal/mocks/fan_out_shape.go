package mocks

import (
	"context"
	"sync"
)

// fanOutShapePins is the in-memory stand-in for the
// purchase_execution_fan_out_shapes table: the first shape pinned for a lineage
// key is kept and returned to every later caller. The zero value is ready.
type fanOutShapePins struct {
	mu   sync.Mutex
	pins map[string]string
}

func (p *fanOutShapePins) pin(lineageKey, shape string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pinned, ok := p.pins[lineageKey]; ok {
		return pinned
	}
	if p.pins == nil {
		p.pins = make(map[string]string)
	}
	p.pins[lineageKey] = shape
	return shape
}

func (p *fanOutShapePins) get(lineageKey string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pins[lineageKey]
}

// PinExecutionFanOutShape mocks the fan-out shape pin (issue #97).
// PinExecutionFanOutShapeFn takes priority; with no expectation registered it
// behaves like the real store (first writer wins per lineage key), so one mock
// shared across successive attempts of a lineage sees what the first pinned.
func (m *MockConfigStore) PinExecutionFanOutShape(ctx context.Context, lineageKey, shape string) (string, error) {
	m.record("PinExecutionFanOutShape", ctx, lineageKey, shape)
	if m.PinExecutionFanOutShapeFn != nil {
		return m.PinExecutionFanOutShapeFn(ctx, lineageKey, shape)
	}
	if !isExpected(&m.Mock, "PinExecutionFanOutShape") {
		return m.fanOutShapes.pin(lineageKey, shape), nil
	}
	args := m.Called(ctx, lineageKey, shape)
	return args.String(0), args.Error(1)
}

// GetExecutionFanOutShape mocks the pinned-shape read (issue #97).
// GetExecutionFanOutShapeFn takes priority; with no expectation registered it
// reads what PinExecutionFanOutShape's default pinned on this mock.
func (m *MockConfigStore) GetExecutionFanOutShape(ctx context.Context, lineageKey string) (string, error) {
	m.record("GetExecutionFanOutShape", ctx, lineageKey)
	if m.GetExecutionFanOutShapeFn != nil {
		return m.GetExecutionFanOutShapeFn(ctx, lineageKey)
	}
	if !isExpected(&m.Mock, "GetExecutionFanOutShape") {
		return m.fanOutShapes.get(lineageKey), nil
	}
	args := m.Called(ctx, lineageKey)
	return args.String(0), args.Error(1)
}
