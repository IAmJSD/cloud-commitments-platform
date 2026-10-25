package goroutines

import (
	"testing"
)

func parkedChild(release <-chan struct{}) { <-release }

func spawnGrandchild(release <-chan struct{}, started chan<- struct{}) {
	go parkedChild(release)
	close(started)
	<-release
}

func TestMarkSpawnedSeesUnscheduledAndNestedGoroutines(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	mark := NewMark()
	if got := mark.Spawned(); len(got) != 0 {
		t.Fatalf("nothing started yet, got %d spawned goroutines", len(got))
	}

	// Read the snapshot right after the go statement, before anything can
	// have scheduled the child: it must already be in the dump. (Its frames
	// may only name the compiler's go-statement wrapper until it runs, so
	// match on the creator rather than on the function name.)
	go parkedChild(release)
	if got := mark.Spawned(); len(got) != 1 {
		t.Fatalf("want the just-started child in the snapshot, got %d", len(got))
	}

	started := make(chan struct{})
	go spawnGrandchild(release, started)
	<-started

	if by := mark.StartedBy("goroutines.spawnGrandchild"); len(by) != 1 {
		t.Fatalf("want the grandchild by its creator, got %d", len(by))
	}

	got := mark.Spawned()
	if len(got) != 3 {
		t.Fatalf("want child, middle and grandchild, got %d: %+v", len(got), got)
	}
	self := CurrentID()
	for _, g := range got {
		if g.ID == self || g.CreatorID == 0 {
			t.Fatalf("unexpected entry %+v", g)
		}
	}
}

func TestMarkStartedIgnoresGoroutinesAliveAtTheMark(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	go parkedChild(release)

	mark := NewMark()
	if got := mark.Spawned(); len(got) != 0 {
		t.Fatalf("a goroutine alive at the mark must not count as started after it, got %d", len(got))
	}
}

func TestParse(t *testing.T) {
	block := "goroutine 42 [chan receive]:\nmain.work()\n\t/x/main.go:9 +0x1d\ncreated by main.main in goroutine 1\n\t/x/main.go:5 +0x25"
	g, ok := parse(block)
	if !ok || g.ID != 42 || g.CreatorID != 1 || g.CreatedBy != "main.main" {
		t.Fatalf("parse = %+v, %v", g, ok)
	}
	if _, ok := parse("not a goroutine"); ok {
		t.Fatal("parse accepted a non-goroutine block")
	}
	if g, _ := parse("goroutine 1 [running]:\nmain.main()"); g.CreatorID != 0 || g.CreatedBy != "" {
		t.Fatalf("main goroutine has no creator, got %+v", g)
	}
}
