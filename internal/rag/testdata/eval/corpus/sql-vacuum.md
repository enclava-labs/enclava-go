# Table bloat and vacuuming

In PostgreSQL, updating or deleting a row does not free its storage immediately. Because a transaction sees a snapshot of the database, the old version of the row must remain until no snapshot can still need it. Autovacuum regularly reclaims this space: it marks dead tuples reusable so later writes occupy the same pages instead of growing the file. The table file is not necessarily returned to the operating system, but the reclaimed room serves new rows.

When dead tuples accumulate faster than vacuum runs, the table bloats. Indexes still point at every dead version, scans read pages of unusable data, and queries slow down steadily even though the visible row count stays flat. Tables receiving heavy updates are the usual suspects; an aggressive autovacuum setting for one hot table is often justified.

VACUUM FULL rewrites the table into a new compacted file and truly shrinks it on disk, but it takes an exclusive lock that blocks reads and writes for the whole operation. Reserve it for emergencies after a runaway batch of updates, and prefer steady prevention: confirm the autovacuum launcher is running, tune its trigger thresholds for update-heavy tables, and watch the dead tuple count rather than file size alone. Long open transactions also hold back cleanup, because their snapshots pin old row versions.
