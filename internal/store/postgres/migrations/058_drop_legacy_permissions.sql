-- Permissions are per collection since 054; the source-permission tables and
-- per-connection audience/revision columns are unread. Drop them.

DROP TABLE source_group_memberships;
DROP TABLE source_grants;
DROP TABLE access_snapshots;
DROP TABLE source_groups;
DROP TABLE source_identities;

ALTER TABLE source_objects DROP COLUMN audience;
ALTER TABLE connections DROP COLUMN audience_default;
ALTER TABLE index_generations
    DROP COLUMN acl_revision,
    DROP COLUMN access_revision;
