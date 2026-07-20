package crypto_test

import (
	"strings"
	"testing"

	"github.com/ericfisherdev/nestova/internal/platform/crypto"
)

// TestDefaultParamsAreOWASPRecommended pins the production cost parameters.
//
// The parameters became injectable so tests could hash cheaply; this guards the
// other direction — that a cheap value can never silently become the default
// and weaken every password in the database. Changing these constants should be
// a deliberate, reviewed act, which failing this test forces.
func TestDefaultParamsAreOWASPRecommended(t *testing.T) {
	t.Parallel()
	got := crypto.DefaultParams()
	want := crypto.Params{
		Time:    1,
		Memory:  64 * 1024, // 64 MiB
		Threads: 4,
	}
	if got != want {
		t.Errorf("DefaultParams() = %+v, want %+v", got, want)
	}
}

// TestPackageLevelHashUsesDefaultParams asserts the package-level Hash — the
// path production code takes when it does not inject a hasher — encodes the
// default cost, not some other hasher's.
func TestPackageLevelHashUsesDefaultParams(t *testing.T) {
	t.Parallel()
	h, err := crypto.Hash("hunter2")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if want := "$m=65536,t=1,p=4$"; !strings.Contains(h, want) {
		t.Errorf("Hash did not encode the default cost parameters %q: %q", want, h)
	}
}

// TestVerifyAcceptsPreExistingProductionHash pins backward compatibility with
// hashes written before the cost parameters became injectable, when Hash derived
// them from compiled-in constants. Credentials already stored in the database
// must keep verifying.
//
// The literal is a real argon2id hash of "hunter2" at m=65536,t=1,p=4. It is
// intentionally hard-coded rather than generated at test time: a generated
// fixture would be produced by the same code under test and so could not detect
// a change to the encoding.
func TestVerifyAcceptsPreExistingProductionHash(t *testing.T) {
	t.Parallel()
	const stored = "$argon2id$v=19$m=65536,t=1,p=4$" +
		"wxsj6Vxnw4oAF00s59pVdA$" +
		"cWwaI5t0loZiPu5dxnx+MHo91ORGkGmzgEJskPSBlU0"

	ok, err := crypto.Verify("hunter2", stored)
	if err != nil {
		t.Fatalf("Verify returned an error for a valid stored hash: %v", err)
	}
	if !ok {
		t.Error("Verify rejected a hash written by the pre-refactor code")
	}

	ok, err = crypto.Verify("wrong", stored)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Error("Verify accepted the wrong password against a stored hash")
	}
}

// TestCheapParamsRoundTripThroughDefaultVerify is the property the whole change
// rests on: Verify reads the cost out of the PHC string rather than assuming the
// compiled-in default, so a cheaply-hashed test fixture verifies through exactly
// the same path as a production hash — and a production hash still verifies even
// when checked by a cheaply-configured hasher.
func TestCheapParamsRoundTripThroughDefaultVerify(t *testing.T) {
	t.Parallel()
	cheap := crypto.NewHasher(crypto.Params{Time: 1, Memory: 64, Threads: 1})

	cheapHash, err := cheap.Hash("hunter2")
	if err != nil {
		t.Fatalf("cheap Hash: %v", err)
	}
	if !strings.Contains(cheapHash, "$m=64,t=1,p=1$") {
		t.Fatalf("cheap hash did not encode its own parameters: %q", cheapHash)
	}

	// A cheaply-produced hash verifies through the package-level (default) path.
	ok, err := crypto.Verify("hunter2", cheapHash)
	if err != nil || !ok {
		t.Errorf("default Verify rejected a cheaply-hashed password: ok=%v err=%v", ok, err)
	}
	ok, err = crypto.Verify("wrong", cheapHash)
	if err != nil || ok {
		t.Errorf("default Verify accepted a wrong password: ok=%v err=%v", ok, err)
	}

	// ...and a default-cost hash verifies through the cheap hasher.
	prodHash, err := crypto.Hash("hunter2")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	ok, err = cheap.Verify("hunter2", prodHash)
	if err != nil || !ok {
		t.Errorf("cheap Verify rejected a default-cost hash: ok=%v err=%v", ok, err)
	}
}
