package adapter

import (
	"testing"
	"time"
)

func TestPerKeyLimiter_AllowsUpToBurstThenBlocks(t *testing.T) {
	l := newPerKeyLimiter(time.Hour, 3) // long refill so the burst is the only budget in play
	key := "member-1"

	for i := 0; i < 3; i++ {
		if !l.allow(key) {
			t.Fatalf("allow() call %d = false, want true (within burst)", i+1)
		}
	}
	if l.allow(key) {
		t.Error("allow() after exhausting the burst = true, want false")
	}
}

func TestPerKeyLimiter_KeysAreIndependent(t *testing.T) {
	l := newPerKeyLimiter(time.Hour, 1)

	if !l.allow("member-1") {
		t.Fatal("first allow() for member-1 = false, want true")
	}
	if l.allow("member-1") {
		t.Error("second allow() for member-1 = true, want false (burst exhausted)")
	}
	if !l.allow("member-2") {
		t.Error("first allow() for member-2 = false, want true (independent bucket)")
	}
}
