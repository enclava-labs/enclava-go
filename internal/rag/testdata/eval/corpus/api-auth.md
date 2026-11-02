# Credentials for API access

APIs distinguish who the caller is from what it may do. A service account is a non-human identity assigned to a workload, so automation does not run as a person; a personal access token is a user-issued credential acting with that user's authority, useful for ad-hoc scripts but dangerous when it outlives its purpose. OAuth 2.0 addresses the delegation problem more formally: an authorization server issues short-lived access tokens to a client, scoped to particular resources, so the third party never sees the user's password.

Scopes are the granular permission list attached to the grant — read repositories, write issues — and the consent screen or admin approval records that the caller accepted them. Access tokens should expire quickly, with refresh tokens or client-credential flows obtaining replacements without storing long-lived secrets in code.

Server-to-server calls commonly use signed JWTs or mutual TLS instead of bearer secrets. Whatever the scheme, the operating rules are the same: transmit credentials only over TLS, keep them out of URLs and logs, scope them to the smallest needed rights, rotate them on a schedule rather than after a suspected leak, and record which credential made each call so revocation is targeted. A leaked token should mean disabling one credential, not rebuilding the integration.
