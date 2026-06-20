-- +goose Up
-- Subscriptions schema (NES-6 / NES-63): recurring household expenses billed on
-- a cycle, with an optional payer attribution. Tenant isolation follows the
-- composite-FK pattern from 00009 — household_id on the table, the payer
-- referencing member(household_id, id) so a subscription can only attribute a
-- member in its own household.

CREATE TABLE subscription (
    id                 uuid        PRIMARY KEY,
    household_id       uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    -- Reject empty AND whitespace-only names so the schema matches
    -- Subscription.Validate, which trims before the blank check.
    name               text        NOT NULL CHECK (name !~ '^[[:space:]]*$'),
    -- Per-cycle cost in the currency's minor unit; strictly positive (a
    -- subscription with no cost is not a subscription). Mirrors the domain
    -- Subscription.Validate amount > 0 rule; Money alone permits zero.
    amount_cents       bigint      NOT NULL CHECK (amount_cents > 0),
    -- ISO-4217 alphabetic code, three uppercase letters (e.g. 'USD').
    currency           char(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    cycle              text        NOT NULL
        CHECK (cycle IN ('weekly', 'monthly', 'yearly', 'custom')),
    next_renewal_on    date        NOT NULL,
    -- NULL when the cost is not attributed to a member; otherwise the payer.
    payer_id           uuid,
    -- Free-text grouping (e.g. "entertainment", "utilities"); '' means uncategorized.
    category           text        NOT NULL DEFAULT '',
    -- Days before next_renewal_on that a renewal reminder should be emitted.
    reminder_lead_days int         NOT NULL DEFAULT 0 CHECK (reminder_lead_days >= 0),
    active             boolean     NOT NULL DEFAULT true,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    -- Lets future child tables compose a tenant FK on (household_id, id).
    CONSTRAINT subscription_household_id_uniq UNIQUE (household_id, id),
    -- Tenant consistency: an attributed payer must belong to the same household.
    -- payer_id is nullable; with MATCH SIMPLE a NULL payer_id skips this check.
    -- ON DELETE SET NULL (payer_id) nulls only the attribution column (not
    -- household_id, which is NOT NULL) when a member is removed, preserving the
    -- subscription — the same column-specific SET NULL pattern usage_event uses
    -- in 00009.
    CONSTRAINT subscription_payer_fk FOREIGN KEY (household_id, payer_id)
        REFERENCES member (household_id, id) ON DELETE SET NULL (payer_id)
);

-- Supports ListActiveByHousehold; partial so it stays small as subscriptions
-- are deactivated.
CREATE INDEX subscription_household_active_idx
    ON subscription (household_id) WHERE active = true;

-- Supports the due-for-renewal scan (ListDueForRenewal), which filters active
-- subscriptions by next_renewal_on within their lead window. Partial (active
-- only) and ordered so the scan stays small and needs no sort.
CREATE INDEX subscription_active_renewal_idx
    ON subscription (next_renewal_on) WHERE active = true;

-- +goose Down
DROP TABLE IF EXISTS subscription;
