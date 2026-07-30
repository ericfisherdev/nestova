-- +goose Up
-- Nestorage member link (NSTR-107): the record connecting one household
-- member to the Nestorage account NSTR-101's federation endpoints found or
-- created for them on the household's bound instance
-- (federation_instance_link, 00038). ReconciliationService.Confirm writes
-- this row only after a successful provision call — Proposals itself never
-- writes here (a proposal is a suggestion, never a decision).
--
-- nestorage_member_link is a 1:1 extension of member (member_id is both the
-- primary key and, via the composite FK below, tenant-checked against
-- household_id), mirroring member_mfa's (00031) own pattern: at most one
-- linked Nestorage account per member.
--
-- remote_user_id is UNIQUE, not just indexed: the reconciliation contract
-- is one member per remote account in BOTH directions
-- (domain.ProposeMatches' own bidirectional-uniqueness rule), and this is
-- the storage-level backstop for the remote-account half of it — the
-- member half is already covered by member_id being this table's own
-- primary key.
--
-- linked_via records why the link was made: a confirmed automatic email
-- match ('email'), a human manually pairing an otherwise-unmatched member
-- ('manual'), or a brand new account Confirm provisioned for a confirmed
-- no-match ('created'). Purely descriptive today — no query in this ticket
-- branches on it.
CREATE TABLE nestorage_member_link (
    member_id      uuid        PRIMARY KEY,
    household_id   uuid        NOT NULL REFERENCES household (id) ON DELETE CASCADE,
    remote_user_id text        NOT NULL CHECK (remote_user_id !~ '^[[:space:]]*$'),
    linked_via     text        NOT NULL CHECK (linked_via IN ('email', 'manual', 'created')),
    linked_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT nestorage_member_link_remote_user_id_uniq UNIQUE (remote_user_id),
    -- Tenant consistency: the linked member must belong to household_id,
    -- mirroring member_mfa_member_fk (00031) and
    -- member_credential_member_fk (00034).
    CONSTRAINT nestorage_member_link_member_fk FOREIGN KEY (household_id, member_id)
        REFERENCES member (household_id, id) ON DELETE CASCADE
);

-- Supports ReconciliationService.Proposals' and Confirm's own
-- household-scoped reads of existing links.
CREATE INDEX nestorage_member_link_household_idx ON nestorage_member_link (household_id);

-- +goose Down
DROP TABLE IF EXISTS nestorage_member_link;
