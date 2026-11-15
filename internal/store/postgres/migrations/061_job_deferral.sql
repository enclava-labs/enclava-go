-- Resilient ingest: jobs requeued WITHOUT consuming an attempt while a
-- dependency (extraction worker) is unavailable. deferrals counts those
-- requeues, first_deferred_at anchors the continuous-wait bound
-- (EXTRACT_WORKER_MAX_WAIT) and drop_deferrals counts the subset where the
-- dependency dropped the connection mid-request (the input may be what
-- crashed it — ingest caps those before they loop forever).
ALTER TABLE jobs
    ADD COLUMN deferrals INT NOT NULL DEFAULT 0,
    ADD COLUMN drop_deferrals INT NOT NULL DEFAULT 0,
    ADD COLUMN first_deferred_at TIMESTAMPTZ NULL;
