-- +goose Up
-- Login enforcement (NES-135): the durable TOTP replay guard for the
-- pre-auth login MFA step. last_totp_step is the RFC 6238 step (the
-- counter, floor(unix_time / 30s)) of the most recently ACCEPTED login TOTP
-- code, or NULL when the member has never completed login MFA verification.
--
-- This must be a single durable, database-backed value rather than
-- something carried in the scs session (00031's original enrollment schema
-- deliberately deferred this — see its own doc comment): a member's phone
-- and laptop have independent sessions, so a per-session guard would let the
-- SAME code be replayed once per device/session. Because the +/-1 step skew
-- window (internal/platform/totp.Provider, mirroring the enrollment
-- confirm/disenroll flow's own tolerance) accepts up to three adjacent
-- steps, a code is rejected as a replay whenever its step is <= the stored
-- value — not just on an exact match — so a stale code from an earlier step
-- in the window cannot be reused even after a later step has already been
-- accepted.
ALTER TABLE member_mfa ADD COLUMN last_totp_step bigint;

-- +goose Down
ALTER TABLE member_mfa DROP COLUMN IF EXISTS last_totp_step;
