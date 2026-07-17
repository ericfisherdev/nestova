package domain_test

import (
	"errors"
	"testing"
	"time"

	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/media/domain"
)

func validTaskInstancePhoto() domain.TaskInstancePhoto {
	return domain.TaskInstancePhoto{
		ID:             domain.NewTaskInstancePhotoID(),
		HouseholdID:    household.NewHouseholdID(),
		TaskInstanceID: domain.TaskInstanceID{},
		Kind:           domain.PhotoKindBefore,
		StorageRef:     domain.StorageRef("hh/chore-photos/ab/abc123.jpg"),
		ContentHash:    validSha256Hex,
		SizeBytes:      1024,
		ContentType:    domain.ContentTypeJPEG,
		TakenAt:        time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC),
	}
}

func TestTaskInstancePhotoValidate(t *testing.T) {
	ok := validTaskInstancePhoto()
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid photo rejected: %v", err)
	}

	for _, ref := range []domain.StorageRef{"", "   "} {
		p := ok
		p.StorageRef = ref
		if !errors.Is(p.Validate(), domain.ErrInvalidTaskInstancePhoto) {
			t.Fatalf("blank storage ref %q accepted", ref)
		}
	}

	invalidHashes := []string{"", "deadbeef", "g" + validSha256Hex[1:]}
	for _, hash := range invalidHashes {
		p := ok
		p.ContentHash = hash
		if !errors.Is(p.Validate(), domain.ErrInvalidTaskInstancePhoto) {
			t.Fatalf("invalid content hash %q accepted", hash)
		}
	}

	for _, size := range []int64{0, -1} {
		p := ok
		p.SizeBytes = size
		if !errors.Is(p.Validate(), domain.ErrInvalidTaskInstancePhoto) {
			t.Fatalf("non-positive size bytes %d accepted", size)
		}
	}

	for _, ct := range []string{"", "image/heic"} {
		p := ok
		p.ContentType = ct
		if !errors.Is(p.Validate(), domain.ErrInvalidTaskInstancePhoto) {
			t.Fatalf("unaccepted content type %q accepted", ct)
		}
	}

	p := ok
	p.Kind = domain.PhotoKindUnspecified
	if !errors.Is(p.Validate(), domain.ErrInvalidTaskInstancePhoto) {
		t.Fatal("unspecified kind accepted")
	}

	p = ok
	p.TakenAt = time.Time{}
	if !errors.Is(p.Validate(), domain.ErrInvalidTaskInstancePhoto) {
		t.Fatal("zero taken_at accepted")
	}
}

func TestPhotoKindValidAndString(t *testing.T) {
	if !domain.PhotoKindBefore.Valid() || domain.PhotoKindBefore.String() != "before" {
		t.Fatalf("PhotoKindBefore = valid:%v string:%q", domain.PhotoKindBefore.Valid(), domain.PhotoKindBefore.String())
	}
	if !domain.PhotoKindAfter.Valid() || domain.PhotoKindAfter.String() != "after" {
		t.Fatalf("PhotoKindAfter = valid:%v string:%q", domain.PhotoKindAfter.Valid(), domain.PhotoKindAfter.String())
	}
	if domain.PhotoKindUnspecified.Valid() {
		t.Fatal("PhotoKindUnspecified must be invalid")
	}
	if domain.PhotoKind(99).Valid() {
		t.Fatal("an unknown PhotoKind must be invalid")
	}
}

func TestParsePhotoKind(t *testing.T) {
	tests := []struct {
		in   string
		want domain.PhotoKind
		ok   bool
	}{
		{"before", domain.PhotoKindBefore, true},
		{"  Before  ", domain.PhotoKindBefore, true},
		{"AFTER", domain.PhotoKindAfter, true},
		{"after", domain.PhotoKindAfter, true},
		{"", domain.PhotoKindUnspecified, false},
		{"sideways", domain.PhotoKindUnspecified, false},
	}
	for _, tt := range tests {
		got, err := domain.ParsePhotoKind(tt.in)
		if tt.ok {
			if err != nil || got != tt.want {
				t.Errorf("ParsePhotoKind(%q) = %v, %v; want %v, nil", tt.in, got, err, tt.want)
			}
			continue
		}
		if !errors.Is(err, domain.ErrInvalidTaskInstancePhoto) {
			t.Errorf("ParsePhotoKind(%q) error = %v, want ErrInvalidTaskInstancePhoto", tt.in, err)
		}
	}
}

func TestAfterPrecedesBefore(t *testing.T) {
	before := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		after time.Time
		want  bool
	}{
		{"after is earlier than before", before.Add(-1 * time.Minute), true},
		{"after equals before", before, false},
		{"after is later than before", before.Add(1 * time.Minute), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domain.AfterPrecedesBefore(tt.after, before); got != tt.want {
				t.Errorf("AfterPrecedesBefore(%s, %s) = %v, want %v", tt.after, before, got, tt.want)
			}
		})
	}
}

func TestTaskInstanceIDRoundTrip(t *testing.T) {
	original := "550e8400-e29b-41d4-a716-446655440000"
	id, err := domain.ParseTaskInstanceID(original)
	if err != nil {
		t.Fatalf("ParseTaskInstanceID: %v", err)
	}
	if id.String() != original {
		t.Fatalf("TaskInstanceID round trip = %q, want %q", id.String(), original)
	}
	if _, err := domain.ParseTaskInstanceID("not-a-uuid"); err == nil {
		t.Fatal("ParseTaskInstanceID accepted a malformed string")
	}
}
