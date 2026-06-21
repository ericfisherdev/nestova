package domain_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

func TestNewRotationInterval(t *testing.T) {
	r, err := domain.NewRotationInterval(8)
	if err != nil {
		t.Fatalf("NewRotationInterval(8): %v", err)
	}
	if r.Seconds() != 8 || r.Duration() != 8*time.Second {
		t.Fatalf("interval = %ds/%s, want 8s", r.Seconds(), r.Duration())
	}
	for _, bad := range []int{0, -1} {
		if _, err := domain.NewRotationInterval(bad); !errors.Is(err, domain.ErrInvalidAlbum) {
			t.Fatalf("NewRotationInterval(%d) error = %v, want ErrInvalidAlbum", bad, err)
		}
	}
}

func TestAlbumValidate(t *testing.T) {
	rot, _ := domain.NewRotationInterval(10)
	ok := domain.Album{ID: domain.NewAlbumID(), HouseholdID: household.NewHouseholdID(), Name: "Family", Rotation: rot}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid album rejected: %v", err)
	}
	// Blank name.
	blank := ok
	blank.Name = "   "
	if !errors.Is(blank.Validate(), domain.ErrInvalidAlbum) {
		t.Fatal("blank name accepted")
	}
	// Zero-value (direct struct construction) rotation is rejected.
	noRot := domain.Album{Name: "X"}
	if !errors.Is(noRot.Validate(), domain.ErrInvalidAlbum) {
		t.Fatal("zero rotation accepted")
	}
}

func TestAlbumFilterJSONRoundTrip(t *testing.T) {
	m1, m2 := household.NewMemberID(), household.NewMemberID()
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := domain.AlbumFilter{MemberIDs: []household.MemberID{m1, m2}, Since: &since}

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out domain.AlbumFilter
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(out.MemberIDs) != 2 || out.MemberIDs[0] != m1 || out.MemberIDs[1] != m2 {
		t.Fatalf("member ids = %v, want [%s %s]", out.MemberIDs, m1, m2)
	}
	if out.Since == nil || !out.Since.Equal(since) || out.Until != nil {
		t.Fatalf("bounds round-trip wrong: since=%v until=%v", out.Since, out.Until)
	}

	// An empty filter serializes to an empty object and matches everything.
	empty, _ := json.Marshal(domain.AlbumFilter{})
	if string(empty) != "{}" {
		t.Fatalf("empty filter = %s, want {}", empty)
	}
	// A malformed member id is rejected on unmarshal.
	if err := json.Unmarshal([]byte(`{"member_ids":["nope"]}`), &out); err == nil {
		t.Fatal("malformed member id accepted")
	}
}

func TestAlbumFilterMatches(t *testing.T) {
	m1, m2 := household.NewMemberID(), household.NewMemberID()
	jan := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	mar := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	photo := func(uploader *household.MemberID, taken *time.Time) domain.Photo {
		return domain.Photo{UploadedBy: uploader, TakenAt: taken}
	}

	// Empty filter matches everything, including a photo with no metadata.
	if !(domain.AlbumFilter{}).Matches(photo(nil, nil)) {
		t.Fatal("empty filter should match a bare photo")
	}
	// Member filter.
	mf := domain.AlbumFilter{MemberIDs: []household.MemberID{m1}}
	if !mf.Matches(photo(&m1, nil)) || mf.Matches(photo(&m2, nil)) || mf.Matches(photo(nil, nil)) {
		t.Fatal("member filter mismatch")
	}
	// Date range filter (inclusive); a photo with no TakenAt is excluded.
	since, until := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	df := domain.AlbumFilter{Since: &since, Until: &until}
	if !df.Matches(photo(nil, &jan)) {
		t.Fatal("in-range photo excluded")
	}
	if df.Matches(photo(nil, &mar)) {
		t.Fatal("out-of-range photo included")
	}
	if df.Matches(photo(nil, nil)) {
		t.Fatal("photo with no TakenAt should be excluded when a date bound is set")
	}
	// Bounds are inclusive: a photo taken exactly at Since or Until matches.
	if !df.Matches(photo(nil, &since)) {
		t.Fatal("photo taken exactly at Since should match (inclusive lower bound)")
	}
	if !df.Matches(photo(nil, &until)) {
		t.Fatal("photo taken exactly at Until should match (inclusive upper bound)")
	}
}
