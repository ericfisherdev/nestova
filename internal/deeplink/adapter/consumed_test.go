package adapter

import (
	"testing"
	"time"
)

func TestConsumedSignatureStore_FirstConsumeSucceedsSecondFails(t *testing.T) {
	s := newConsumedSignatureStore()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	expiry := now.Add(10 * time.Minute).Unix()

	if !s.consume("sig-a", expiry, now) {
		t.Fatal("first consume() = false, want true")
	}
	if s.consume("sig-a", expiry, now) {
		t.Error("second consume() of the same signature = true, want false")
	}
}

func TestConsumedSignatureStore_DistinctSignaturesAreIndependent(t *testing.T) {
	s := newConsumedSignatureStore()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	expiry := now.Add(10 * time.Minute).Unix()

	if !s.consume("sig-a", expiry, now) {
		t.Fatal("consume(sig-a) = false, want true")
	}
	if !s.consume("sig-b", expiry, now) {
		t.Error("consume(sig-b) = false, want true (independent signature)")
	}
}

func TestConsumedSignatureStore_SweepsExpiredEntries(t *testing.T) {
	s := newConsumedSignatureStore()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	shortExpiry := now.Add(time.Minute).Unix()

	if !s.consume("sig-a", shortExpiry, now) {
		t.Fatal("consume(sig-a) = false, want true")
	}

	// Once the recorded link's own expiry has passed, the signature would
	// already fail verification on its own — the store no longer needs to
	// remember it, and a re-consume at this point must succeed (a NEW link
	// reusing the same signature string is not realistically possible since
	// signatures are HMACs over path+expiry, but the store's own sweep
	// behavior is independently testable here).
	later := now.Add(2 * time.Minute)
	if !s.consume("sig-a", shortExpiry, later) {
		t.Error("consume(sig-a) after its recorded expiry = false, want true (swept)")
	}
}

func TestConsumedSignatureStore_ConcurrentConsumeIsExactlyOnceWinner(t *testing.T) {
	s := newConsumedSignatureStore()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	expiry := now.Add(10 * time.Minute).Unix()

	const attempts = 50
	results := make(chan bool, attempts)
	for i := 0; i < attempts; i++ {
		go func() {
			results <- s.consume("sig-race", expiry, now)
		}()
	}
	successes := 0
	for i := 0; i < attempts; i++ {
		if <-results {
			successes++
		}
	}
	if successes != 1 {
		t.Errorf("concurrent consume() calls on the same signature: %d succeeded, want exactly 1", successes)
	}
}
