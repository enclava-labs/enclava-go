-- Dedicated tinfoil enclaves verify against an explicit repository
-- reference; the shared router host uses the SDK's router configuration
-- instead, so the column stays empty there. Enforced at the transport
-- boundary: a dedicated enclave without a repo fails closed, never silently
-- unverified.
ALTER TABLE inference_endpoints
    ADD COLUMN enclave_repo TEXT NOT NULL DEFAULT '';
