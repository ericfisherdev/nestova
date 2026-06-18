package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	authadapter "github.com/ericfisherdev/nestova/internal/auth/adapter"
	householdadapter "github.com/ericfisherdev/nestova/internal/household/adapter"
	household "github.com/ericfisherdev/nestova/internal/household/domain"
)

// onboardingAdvisoryLock is a fixed key for the transaction-scoped advisory lock
// that serializes first-run household provisioning across connections.
const onboardingAdvisoryLock int64 = 0x4E45535F4F4E42 // "NES_ONB"

// txProvisioner implements authadapter.Provisioner by running each multi-table
// write inside a single pgx transaction. It lives in the composition root
// because it depends on both the household adapter and the auth adapter; keeping
// it here means neither bounded-context adapter imports the other.
type txProvisioner struct {
	pool *pgxpool.Pool
}

// Compile-time assurance the provisioner satisfies the port.
var _ authadapter.Provisioner = (*txProvisioner)(nil)

// newTxProvisioner constructs a transactional provisioner over the shared pool.
func newTxProvisioner(pool *pgxpool.Pool) *txProvisioner {
	if pool == nil {
		panic("main: newTxProvisioner requires a non-nil pool")
	}
	return &txProvisioner{pool: pool}
}

// ProvisionHousehold creates the household, adds the owner member, and stores
// the owner's credentials in one transaction. Any error rolls back the whole
// unit of work, so onboarding never leaves an orphaned household or a member
// without credentials. Domain errors from the tx-scoped repositories
// (ErrDuplicateMember, ErrEmailAlreadyInUse) surface unchanged.
func (p *txProvisioner) ProvisionHousehold(
	ctx context.Context,
	hh *household.Household,
	owner *household.Member,
	email, passwordHash string,
) error {
	if owner.HouseholdID != hh.ID {
		return fmt.Errorf("provision household: owner household %v does not match household %v", owner.HouseholdID, hh.ID)
	}
	if passwordHash == "" {
		return fmt.Errorf("provision household: owner password hash is required")
	}
	return p.withTx(ctx, func(tx pgx.Tx, hr *householdadapter.PostgresRepository, cr *authadapter.CredentialRepository) error {
		// Serialize concurrent first-run onboarding across connections: a
		// transaction-scoped advisory lock makes the "no household yet" check and
		// the insert atomic, closing the check-then-create race without
		// permanently constraining the schema to a single household.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", onboardingAdvisoryLock); err != nil {
			return fmt.Errorf("acquire onboarding lock: %w", err)
		}
		exists, err := hr.HasAnyHousehold(ctx)
		if err != nil {
			return err
		}
		if exists {
			return household.ErrHouseholdExists
		}
		if err := hr.CreateHousehold(ctx, hh); err != nil {
			return err
		}
		if err := hr.AddMember(ctx, owner); err != nil {
			return err
		}
		return cr.SetPassword(ctx, owner.ID, email, passwordHash)
	})
}

// ProvisionMember adds the member and, when email is non-empty, stores
// credentials in one transaction. An empty email means no credentials are
// written. Domain errors surface unchanged.
func (p *txProvisioner) ProvisionMember(
	ctx context.Context,
	m *household.Member,
	email, passwordHash string,
) error {
	return p.withTx(ctx, func(_ pgx.Tx, hr *householdadapter.PostgresRepository, cr *authadapter.CredentialRepository) error {
		if err := hr.AddMember(ctx, m); err != nil {
			return err
		}
		if email == "" {
			return nil
		}
		if passwordHash == "" {
			return fmt.Errorf("provision member: password hash is required when email is set")
		}
		return cr.SetPassword(ctx, m.ID, email, passwordHash)
	})
}

// withTx begins a transaction, builds tx-scoped repositories (pgx.Tx satisfies
// db.TX), runs fn, and commits on success. The deferred Rollback is a no-op
// after a successful Commit (canonical pgx v5 pattern).
func (p *txProvisioner) withTx(
	ctx context.Context,
	fn func(pgx.Tx, *householdadapter.PostgresRepository, *authadapter.CredentialRepository) error,
) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	hr := householdadapter.NewPostgresRepository(tx)
	cr := authadapter.NewCredentialRepository(tx)

	if err := fn(tx, hr, cr); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
