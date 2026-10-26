// Package goroutines lets tests wait on, or rule out, background goroutines
// without a wall-clock sleep (#36). It reads runtime.Stack(all=true), which
// stops the world, so each snapshot is consistent: a goroutine started by a go
// statement that has already executed is in the snapshot even if the scheduler
// has not run it yet, and it stays there until its function returns.
//
// It is a separate package from testutil, with no dependencies, so packages
// that testutil imports (such as scheduler and purchase) can use it from their
// internal tests without an import cycle.
package goroutines

import (
	"runtime"
	"strconv"
	"strings"
)

// Goroutine is one live goroutine from a stack dump.
type Goroutine struct {
	// Stack is the goroutine's block of the dump: the header, every frame
	// and the "created by" line.
	Stack string
	// ID is the runtime's goroutine ID.
	ID int64
	// CreatedBy is the function that ran the go statement, from the dump's
	// "created by" line, or "" when the dump does not name one (the main
	// goroutine, runtime workers).
	CreatedBy string
	// CreatorID is the ID of the goroutine that ran the go statement, or 0
	// when the dump does not name one.
	CreatorID int64
}

// Snapshot returns every live goroutine, the caller included.
func Snapshot() []Goroutine {
	blocks := strings.Split(string(stackDump(true)), "\n\n")
	out := make([]Goroutine, 0, len(blocks))
	for _, block := range blocks {
		if g, ok := parse(block); ok {
			out = append(out, g)
		}
	}
	return out
}

// CurrentID returns the calling goroutine's ID.
func CurrentID() int64 {
	g, _ := parse(string(stackDump(false)))
	return g.ID
}

// Mark remembers which goroutines were alive, and which goroutine took the
// mark, so goroutines started later can be told apart from older ones.
type Mark struct {
	alive map[int64]bool
	self  int64
}

// NewMark records the goroutines alive now. Goroutine IDs are never reused,
// so a goroutine missing from the mark was started after it.
func NewMark() Mark {
	m := Mark{alive: map[int64]bool{}, self: CurrentID()}
	for _, g := range Snapshot() {
		m.alive[g.ID] = true
	}
	return m
}

// Started returns the live goroutines that were not alive when the mark was
// taken and whose stack contains every one of substrs. Until a goroutine has
// first run, its frames may name only the compiler's wrapper for the go
// statement, so match on frames it blocks in (or use Spawned) rather than on
// its entry function.
func (m Mark) Started(substrs ...string) []Goroutine {
	var out []Goroutine
	for _, g := range Snapshot() {
		if !m.alive[g.ID] && containsAll(g.Stack, substrs) {
			out = append(out, g)
		}
	}
	return out
}

// StartedBy returns the live goroutines that were not alive when the mark was
// taken and whose go statement ran in a function whose name contains creator,
// for example "api.(*DBRateLimiter).StartCleanupWorker". The creator is known
// from the moment the goroutine exists, whether or not it has run yet.
func (m Mark) StartedBy(creator string) []Goroutine {
	var out []Goroutine
	for _, g := range m.Started() {
		if strings.Contains(g.CreatedBy, creator) {
			out = append(out, g)
		}
	}
	return out
}

// Spawned returns the live goroutines started after the mark by the goroutine
// that took it, directly or through another goroutine in the result. A
// goroutine that has already returned is not in the result; a caller ruling
// out background work must also check that work's side effects. Call it from
// the goroutine's own code, not from a callback another helper runs on a
// goroutine of its own (require.Eventually runs its condition that way):
// that helper goroutine would be in the result. Use StartedBy there.
func (m Mark) Spawned() []Goroutine {
	fresh := m.Started()
	owned := map[int64]bool{m.self: true}
	var out []Goroutine
	for grew := true; grew; {
		grew = false
		for _, g := range fresh {
			if !owned[g.ID] && owned[g.CreatorID] {
				owned[g.ID] = true
				out = append(out, g)
				grew = true
			}
		}
	}
	return out
}

func stackDump(all bool) []byte {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, all)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}

// parse reads one dump block: "goroutine 7 [chan receive]:" then frames,
// with "created by pkg.fn in goroutine 1" near the end for a goroutine
// started by a go statement.
func parse(block string) (Goroutine, bool) {
	block = strings.TrimSpace(block)
	rest, ok := strings.CutPrefix(block, "goroutine ")
	if !ok {
		return Goroutine{}, false
	}
	idText, _, ok := strings.Cut(rest, " ")
	if !ok {
		return Goroutine{}, false
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return Goroutine{}, false
	}
	g := Goroutine{ID: id, Stack: block}
	g.CreatedBy, g.CreatorID = creator(block)
	return g, true
}

// creator reads "created by pkg.fn in goroutine 1" from a dump block.
func creator(block string) (fn string, id int64) {
	for _, line := range strings.Split(block, "\n") {
		rest, ok := strings.CutPrefix(line, "created by ")
		if !ok {
			continue
		}
		fn, idText, ok := strings.Cut(rest, " in goroutine ")
		if !ok {
			return rest, 0
		}
		id, err := strconv.ParseInt(idText, 10, 64)
		if err != nil {
			return fn, 0
		}
		return fn, id
	}
	return "", 0
}

func containsAll(s string, substrs []string) bool {
	for _, sub := range substrs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
