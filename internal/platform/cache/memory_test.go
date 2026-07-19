package cache_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/cache"
)

func TestMemoryCache_GetMiss_ReturnsOkFalse(t *testing.T) {
	c := cache.NewMemoryCache()
	value, ok, err := c.Get(context.Background(), "notify:missing:1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Error("Get(missing key) ok = true, want false")
	}
	if value != nil {
		t.Errorf("Get(missing key) value = %v, want nil", value)
	}
}

func TestMemoryCache_SetThenGet_RoundTrips(t *testing.T) {
	c := cache.NewMemoryCache()
	want := []byte(`{"hello":"world"}`)
	if err := c.Set(context.Background(), "recipes:externalfind:abc", want, time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := c.Get(context.Background(), "recipes:externalfind:abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get ok = false, want true")
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Get value = %q, want %q", got, want)
	}
}

func TestMemoryCache_Set_StoresACopy_NotAliasedToTheCallersSlice(t *testing.T) {
	c := cache.NewMemoryCache()
	original := []byte("original")
	if err := c.Set(context.Background(), "k", original, time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Mutate the caller's own slice after Set returns.
	original[0] = 'X'

	got, ok, err := c.Get(context.Background(), "k")
	if err != nil || !ok {
		t.Fatalf("Get = (%v, %v, %v)", got, ok, err)
	}
	if string(got) != "original" {
		t.Errorf("Get value = %q, want %q (Set must copy, not alias, the caller's slice)", got, "original")
	}
}

func TestMemoryCache_Get_ReturnsACopy_MutatingItDoesNotAffectTheCache(t *testing.T) {
	c := cache.NewMemoryCache()
	if err := c.Set(context.Background(), "k", []byte("stored"), time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, _, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got[0] = 'X'

	got2, _, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get (second): %v", err)
	}
	if string(got2) != "stored" {
		t.Errorf("second Get value = %q, want %q (mutating the first Get's result must not affect the cache)", got2, "stored")
	}
}

func TestMemoryCache_ZeroTTL_NeverExpires(t *testing.T) {
	c := cache.NewMemoryCache()
	if err := c.Set(context.Background(), "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// A zero TTL entry should still be present well after any bounded TTL
	// would have expired; there is no time to wait out here since it never
	// expires by construction.
	_, ok, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Error("Get ok = false, want true (zero TTL means no expiration)")
	}
}

func TestMemoryCache_ExpiredEntry_IsTreatedAsMiss(t *testing.T) {
	c := cache.NewMemoryCache()
	if err := c.Set(context.Background(), "k", []byte("v"), time.Millisecond); err != nil {
		t.Fatalf("Set: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	_, ok, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Error("Get ok = true, want false (entry should have expired)")
	}
}

func TestMemoryCache_Set_Overwrites(t *testing.T) {
	c := cache.NewMemoryCache()
	if err := c.Set(context.Background(), "k", []byte("first"), time.Hour); err != nil {
		t.Fatalf("Set (first): %v", err)
	}
	if err := c.Set(context.Background(), "k", []byte("second"), time.Hour); err != nil {
		t.Fatalf("Set (second): %v", err)
	}
	got, ok, err := c.Get(context.Background(), "k")
	if err != nil || !ok {
		t.Fatalf("Get = (%v, %v, %v)", got, ok, err)
	}
	if string(got) != "second" {
		t.Errorf("Get value = %q, want %q", got, "second")
	}
}

func TestMemoryCache_Delete_RemovesTheEntry(t *testing.T) {
	c := cache.NewMemoryCache()
	if err := c.Set(context.Background(), "k", []byte("v"), time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Delete(context.Background(), "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Error("Get ok = true after Delete, want false")
	}
}

func TestMemoryCache_Delete_UnknownKey_IsNotAnError(t *testing.T) {
	c := cache.NewMemoryCache()
	if err := c.Delete(context.Background(), "never-set"); err != nil {
		t.Errorf("Delete(unknown key): %v, want nil", err)
	}
}

func TestMemoryCache_Set_NegativeTTL_ReturnsErrNegativeTTL(t *testing.T) {
	c := cache.NewMemoryCache()
	err := c.Set(context.Background(), "k", []byte("v"), -time.Second)
	if !errors.Is(err, cache.ErrNegativeTTL) {
		t.Fatalf("Set(negative ttl) = %v, want %v", err, cache.ErrNegativeTTL)
	}
	// The rejected Set must not have written anything.
	_, ok, getErr := c.Get(context.Background(), "k")
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if ok {
		t.Error("Get ok = true after a rejected negative-TTL Set, want false")
	}
}
