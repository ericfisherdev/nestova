-- +goose Up
-- Renewal-reminder idempotency (NES-65). reminded_for records the renewal
-- occurrence (a next_renewal_on date) a reminder has already been emitted for, so
-- the renewal scheduler raises exactly one reminder per occurrence even across
-- repeated polling ticks. It is cleared (set NULL) when the renewal advances to a
-- new occurrence. NULL means no reminder has been emitted for the current
-- next_renewal_on yet. A plain nullable column with no default is a non-blocking
-- add (the subscription table carries no historical rows that need backfilling).
ALTER TABLE subscription
    ADD COLUMN reminded_for date;

-- +goose Down
ALTER TABLE subscription
    DROP COLUMN reminded_for;
