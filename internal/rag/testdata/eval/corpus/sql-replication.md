# Streaming replication and lag

Streaming replication ships write-ahead log records from a primary server to each standby, which replays them to remain a live copy. The gap between what the primary has written and what the standby has replayed is the replication lag. A standby can lag while receiving, while replaying, or both; a busy read replica that runs long analytical queries may pause replay to avoid query conflicts, letting lag grow even when the network is fine.

Operators measure lag in bytes of unreplayed log and in seconds since the last received record was produced. Viewing only one measure is risky: a standby that receives steadily but replays slowly shows low receive delay and steadily growing total delay. The replay timestamp on the primary's view of each standby is the usual health signal.

Watch for the downstream effects: a replica falling too far behind can pass the oldest WAL segment still needed, forcing it to be rebuilt from a base backup unless replication slots retain the segments. Slots prevent that loss but accumulate disk usage on the primary while the standby is down. Document the acceptable lag for read routing, alert on its trend rather than its instantaneous value, and fail over only after confirming the replica has replayed the records the primary sent.
