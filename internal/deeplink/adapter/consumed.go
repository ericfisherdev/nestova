package adapter

import (
	"sync"
	"time"
)

// consumedSignatureStore records signatures already consumed by a
// successful, one-shot action — currently only reward redemption (see
// WebHandlers.confirmRedeem) — so a resubmitted POST (double-tap, a browser
// refresh-and-resend, or simply a second request landing within the
// per-member rate limiter's burst) cannot repeat an action that has a real
// side effect beyond a harmless no-op. Claim and complete do NOT use this
// store: see confirmClaim/confirmComplete's doc comments for why re-running
// either is already safe by construction.
//
// It is a small in-process map, not a database table: Nestova is a
// single-process, local-first household appliance (see internal/kiosk's own
// doc comments — there is exactly one server process ever holding this
// state), so the operational cost of a persisted table is not justified by
// what it would buy. The accepted tradeoff: a server restart clears this
// map, which re-opens AT MOST the remaining TTL window of any link consumed
// shortly before the restart — i.e. in the narrow case where a redeem
// succeeds and the process restarts before that specific link's own signed
// expiry, the SAME link could be replayed once more. This is a deliberate,
// bounded, documented choice for a family appliance, not an oversight; a
// persisted table is the natural next step if this tradeoff ever stops being
// acceptable.
type consumedSignatureStore struct {
	mu sync.Mutex
	// consumed maps a signature to the expiry (unix seconds) of the link it
	// authorized, so entries are only ever kept until they would fail
	// signature verification anyway (see the doc above).
	consumed map[string]int64
}

func newConsumedSignatureStore() *consumedSignatureStore {
	return &consumedSignatureStore{consumed: make(map[string]int64)}
}

// consume atomically checks whether signature has already been consumed and,
// if not, marks it consumed. It returns false when signature was already
// consumed as of now — the caller must NOT perform the action a second time
// — and true when this call is the one doing the consuming.
//
// Each call opportunistically sweeps every already-expired entry out of the
// map first. This keeps the map bounded by "signatures currently within
// their TTL window" without a background goroutine/ticker: the household
// appliance's actual redemption volume (a handful of confirms at a time,
// household-scale) makes an O(n) sweep on every call negligible.
func (s *consumedSignatureStore) consume(signature string, expiry int64, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	nowUnix := now.Unix()
	for sig, exp := range s.consumed {
		if exp <= nowUnix {
			delete(s.consumed, sig)
		}
	}

	if exp, ok := s.consumed[signature]; ok && exp > nowUnix {
		return false
	}
	s.consumed[signature] = expiry
	return true
}

// release un-marks signature as consumed. It is used when the action
// consume() was guarding turned out NOT to happen — e.g. the guarded service
// call itself rejected the attempt (a real domain error) or failed
// transiently — so the link never had the real-world side effect the guard
// exists to prevent, and a legitimate retry with the same still-valid link
// should remain possible.
func (s *consumedSignatureStore) release(signature string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.consumed, signature)
}
