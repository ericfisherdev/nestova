-- +goose Up
-- Reverts the NSTR-105/106/107 provider-side federation schema (00037-00039),
-- superseded by NSTR-112's shared identity schema. The original migrations
-- stay on disk: they already ran in environments that deployed main, so
-- goose_db_version records versions 37-39 there, and deleting the files
-- would strand those rows -- breaking `migrate-down`, breaking
-- `migrate-reset` (the orphaned tables keep FKs into member), and silently
-- skipping any future migration renumbered into the 37-39 gap. Dropped in
-- reverse dependency order: nestorage_member_link and
-- federation_authorization_code reference federation_instance_link/member.
DROP TABLE IF EXISTS nestorage_member_link;
DROP TABLE IF EXISTS federation_instance_link;
DROP TABLE IF EXISTS federation_authorization_code;

-- +goose Down
-- Intentionally empty: 00037-00039 own recreating these tables, and this
-- migration's Down must not resurrect a surface the application no longer
-- has any code to serve.
SELECT 1;
