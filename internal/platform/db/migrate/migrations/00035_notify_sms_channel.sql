-- +goose Up
-- SMS notification channel (NES-138): widens notification.channel's CHECK
-- constraint to accept 'sms' alongside the existing push/email/inapp
-- values, so an SMS-routed notification (member phone numbers, preference
-- routing — NES-139) can be enqueued at all. This migration adds ONLY the
-- schema capability: no code in this ticket enqueues a 'sms' row yet (see
-- docs/aws-sms.md).
--
-- The constraint dropped here is the one 00001_baseline.sql declared
-- inline on the column (`channel text NOT NULL CHECK (channel IN (...))`),
-- which PostgreSQL auto-names <table>_<column>_check for an unnamed
-- single-column CHECK — notification_channel_check. Re-added under the
-- SAME name (not left to auto-generate again) so it stays the identical,
-- introspectable name after this migration as before it, mirroring
-- 00024_reward_catalog_admin.sql's drop/re-add-with-a-wider-definition
-- pattern for a constraint that cannot be altered in place.
ALTER TABLE notification DROP CONSTRAINT notification_channel_check;
ALTER TABLE notification
    ADD CONSTRAINT notification_channel_check CHECK (channel IN ('push', 'email', 'inapp', 'sms'));

-- +goose Down
-- 'sms' rows cannot exist in the old schema (the narrowed CHECK below
-- forbids the value), so rolling back removes them — mirrors
-- 00018_task_instance_as_needed.sql's identical delete-before-re-narrow
-- pattern for a value the down migration's own constraint no longer
-- allows.
DELETE FROM notification WHERE channel = 'sms';
ALTER TABLE notification DROP CONSTRAINT notification_channel_check;
ALTER TABLE notification
    ADD CONSTRAINT notification_channel_check CHECK (channel IN ('push', 'email', 'inapp'));
