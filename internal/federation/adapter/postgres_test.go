package adapter_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ericfisherdev/nestova/internal/federation/adapter"
	"github.com/ericfisherdev/nestova/internal/federation/domain"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
	"github.com/ericfisherdev/nestova/internal/platform/db/dbtest"
)

// newTestPool returns a pool against this package's own derived database
// (NES-149), freshly reset and migrated, mirroring kiosk/adapter's own
// helper of the same name.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return dbtest.NewIsolatedPool(t, "federation")
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func seedHousehold(t *testing.T, pool *pgxpool.Pool) household.HouseholdID {
	t.Helper()
	id := household.NewHouseholdID()
	if _, err := pool.Exec(testCtx(t), `INSERT INTO household (id, name) VALUES ($1, $2)`, id.String(), "The Fishers"); err != nil {
		t.Fatalf("seed household: %v", err)
	}
	return id
}

func seedMember(t *testing.T, pool *pgxpool.Pool, hh household.HouseholdID, name string) household.MemberID {
	t.Helper()
	id := household.NewMemberID()
	if _, err := pool.Exec(testCtx(t),
		`INSERT INTO member (id, household_id, display_name, role, color_key) VALUES ($1, $2, $3, 'owner', 'sage')`,
		id.String(), hh.String(), name); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	return id
}

func newLink(hh household.HouseholdID, attachedBy *household.MemberID, baseURL string) *domain.InstanceLink {
	return &domain.InstanceLink{
		ID:          domain.NewInstanceLinkID(),
		HouseholdID: hh,
		BaseURL:     baseURL,
		APIKeyEnc:   []byte("encrypted-key-bytes"),
		AttachedBy:  attachedBy,
	}
}

func TestInstanceLinkRepositoryCreateAndGetByHousehold(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewInstanceLinkRepository(pool)
	hh := seedHousehold(t, pool)
	member := seedMember(t, pool, hh, "Owner")
	ctx := testCtx(t)

	link := newLink(hh, &member, "https://nestorage.example.ts.net")
	if err := repo.Create(ctx, link); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if link.AttachedAt.IsZero() {
		t.Error("Create did not populate AttachedAt")
	}

	got, err := repo.GetByHousehold(ctx, hh)
	if err != nil {
		t.Fatalf("GetByHousehold: %v", err)
	}
	if got.ID != link.ID || got.BaseURL != link.BaseURL {
		t.Fatalf("GetByHousehold = %+v, want matching %+v", got, link)
	}
	if got.AttachedBy == nil || *got.AttachedBy != member {
		t.Errorf("GetByHousehold AttachedBy = %v, want %v", got.AttachedBy, member)
	}

	if _, err := repo.GetByHousehold(ctx, household.NewHouseholdID()); !errors.Is(err, domain.ErrLinkNotFound) {
		t.Errorf("GetByHousehold(unknown) error = %v, want ErrLinkNotFound", err)
	}
}

func TestInstanceLinkRepositoryCreateUnknownHouseholdFails(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewInstanceLinkRepository(pool)
	ctx := testCtx(t)

	link := newLink(household.NewHouseholdID(), nil, "https://orphan.example.ts.net")
	if err := repo.Create(ctx, link); !errors.Is(err, household.ErrHouseholdNotFound) {
		t.Errorf("Create with unknown household error = %v, want ErrHouseholdNotFound", err)
	}
}

func TestInstanceLinkRepositoryCreateRefusesSecondBindingForHousehold(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewInstanceLinkRepository(pool)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	if err := repo.Create(ctx, newLink(hh, nil, "https://first.example.ts.net")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	err := repo.Create(ctx, newLink(hh, nil, "https://second.example.ts.net"))
	if !errors.Is(err, domain.ErrHouseholdAlreadyBound) {
		t.Fatalf("second Create() error = %v, want ErrHouseholdAlreadyBound", err)
	}
}

func TestInstanceLinkRepositoryCreateRefusesURLBoundToAnotherHousehold(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewInstanceLinkRepository(pool)
	first := seedHousehold(t, pool)
	second := seedHousehold(t, pool)
	ctx := testCtx(t)

	if err := repo.Create(ctx, newLink(first, nil, "https://shared.example.ts.net")); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	err := repo.Create(ctx, newLink(second, nil, "https://Shared.Example.ts.net"))
	if !errors.Is(err, domain.ErrInstanceAlreadyBound) {
		t.Fatalf("second Create() (different case, same host) error = %v, want ErrInstanceAlreadyBound", err)
	}
}

func TestInstanceLinkRepositoryGetByBaseURL(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewInstanceLinkRepository(pool)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	link := newLink(hh, nil, "https://nestorage.example.ts.net")
	if err := repo.Create(ctx, link); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// lower(base_url) matching means a differently-cased lookup still finds it.
	got, err := repo.GetByBaseURL(ctx, "https://Nestorage.Example.ts.net")
	if err != nil {
		t.Fatalf("GetByBaseURL: %v", err)
	}
	if got.ID != link.ID {
		t.Errorf("GetByBaseURL = %+v, want id %v", got, link.ID)
	}

	if _, err := repo.GetByBaseURL(ctx, "https://never-attached.example.ts.net"); !errors.Is(err, domain.ErrLinkNotFound) {
		t.Errorf("GetByBaseURL(unknown) error = %v, want ErrLinkNotFound", err)
	}
}

func TestInstanceLinkRepositoryDeleteByHousehold(t *testing.T) {
	pool := newTestPool(t)
	repo := adapter.NewInstanceLinkRepository(pool)
	hh := seedHousehold(t, pool)
	ctx := testCtx(t)

	if err := repo.Create(ctx, newLink(hh, nil, "https://nestorage.example.ts.net")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.DeleteByHousehold(ctx, hh); err != nil {
		t.Fatalf("DeleteByHousehold: %v", err)
	}
	if _, err := repo.GetByHousehold(ctx, hh); !errors.Is(err, domain.ErrLinkNotFound) {
		t.Errorf("GetByHousehold after delete error = %v, want ErrLinkNotFound", err)
	}

	// Deleting an already-unbound (or never-bound) household is a no-op,
	// not an error — Detach is idempotent.
	if err := repo.DeleteByHousehold(ctx, hh); err != nil {
		t.Errorf("DeleteByHousehold on an already-unbound household: %v", err)
	}
}
