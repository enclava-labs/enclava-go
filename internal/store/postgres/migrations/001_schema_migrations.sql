-- Version marker. The schema_migrations table itself is created
-- unconditionally in Go before any migration runs; the runner records this
-- filename as applied inside the migration transaction.
SELECT 1;
