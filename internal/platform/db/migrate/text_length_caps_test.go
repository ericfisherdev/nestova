package migrate_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ericfisherdev/nestova/internal/platform/db/dbtest"
)

// TestTextFieldLengthCaps proves the schema itself bounds every user-entered
// text column (NES-194), not just the domain guards. The domain rejects an
// over-length value first and words a readable error; these constraints are the
// backstop for a caller that writes SQL directly. Each column takes a value of
// exactly its cap and refuses one rune more, in characters rather than bytes.
func TestTextFieldLengthCaps(t *testing.T) {
	pool := dbtest.NewIsolatedPool(t, "textcaps")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	if _, err := pool.Exec(ctx,
		`INSERT INTO identity.household (id, name) VALUES ('11111111-1111-4111-8111-111111111111', 'The Fishers')`); err != nil {
		t.Fatalf("seed household: %v", err)
	}
	const householdID = "'11111111-1111-4111-8111-111111111111'"

	// $1 is the value under test; each statement satisfies every other NOT NULL
	// column so the length CHECK is the only thing that can refuse it.
	cases := []struct {
		constraint string
		cap        int
		insert     string
	}{
		{
			"reward_name_length", 200,
			`INSERT INTO reward (id, household_id, name, cost_points) VALUES (gen_random_uuid(), ` + householdID + `, $1, 10)`,
		},
		{
			"reward_description_length", 1000,
			`INSERT INTO reward (id, household_id, name, cost_points, description) VALUES (gen_random_uuid(), ` + householdID + `, 'Toy', 10, $1)`,
		},
		{
			"album_name_length", 200,
			`INSERT INTO album (id, household_id, name, rotation_seconds) VALUES (gen_random_uuid(), ` + householdID + `, $1, 8)`,
		},
		{
			"recipe_title_length", 200,
			`INSERT INTO recipe (id, household_id, title, source, servings) VALUES (gen_random_uuid(), ` + householdID + `, $1, 'local', 2)`,
		},
		{
			"ingredient_canonical_name_length", 200,
			`INSERT INTO ingredient (id, canonical_name) VALUES (gen_random_uuid(), $1)`,
		},
		{
			"subscription_name_length", 200,
			`INSERT INTO subscription (id, household_id, name, amount_cents, currency, cycle, next_renewal_on)
			 VALUES (gen_random_uuid(), ` + householdID + `, $1, 999, 'USD', 'monthly', '2030-01-01')`,
		},
		{
			"shopping_list_item_name_length", 200,
			`INSERT INTO shopping_list_item (id, household_id, name, quantity, unit, source)
			 VALUES (gen_random_uuid(), ` + householdID + `, $1, 1, 'count', 'manual')`,
		},
		{
			"kiosk_device_name_length", 200,
			`INSERT INTO kiosk_device (id, household_id, token_hash, name) VALUES (gen_random_uuid(), ` + householdID + `, md5(random()::text), $1)`,
		},
		{
			"kiosk_activation_code_name_length", 200,
			`INSERT INTO kiosk_activation_code (id, household_id, code_hash, name, expires_at)
			 VALUES (gen_random_uuid(), ` + householdID + `, md5(random()::text), $1, now() + interval '1 hour')`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.constraint, func(t *testing.T) {
			// A multi-byte value at the cap is the case a byte-based check would
			// wrongly refuse: it is cap characters but three times that in bytes.
			if _, err := pool.Exec(ctx, tc.insert, strings.Repeat("家", tc.cap)); err != nil {
				t.Fatalf("a value of exactly %d characters was refused: %v", tc.cap, err)
			}

			_, err := pool.Exec(ctx, tc.insert, strings.Repeat("家", tc.cap+1))
			if err == nil {
				t.Fatalf("a value of %d characters was accepted; %s must refuse it", tc.cap+1, tc.constraint)
			}
			if !strings.Contains(err.Error(), tc.constraint) {
				t.Errorf("error = %v, want it to name the %s constraint", err, tc.constraint)
			}
		})
	}
}
