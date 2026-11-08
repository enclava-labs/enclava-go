-- GitLab, Jira and Linear sources and their credentials.
ALTER TABLE connections DROP CONSTRAINT connections_kind_check;
ALTER TABLE connections ADD CONSTRAINT connections_kind_check
    CHECK (kind IN ('github','slack','teams','notion','google_drive','gitlab','jira','linear'));

ALTER TABLE connector_credentials DROP CONSTRAINT connector_credentials_provider_check;
ALTER TABLE connector_credentials ADD CONSTRAINT connector_credentials_provider_check
    CHECK (provider IN ('github','slack','notion','teams','google_drive','gitlab','jira','linear'));
