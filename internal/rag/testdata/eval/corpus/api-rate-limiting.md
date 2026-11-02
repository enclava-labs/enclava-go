# Rate limiting requests

A rate limiter decides whether a request may proceed by counting recent demand per client, key or account. The token bucket algorithm stores a small bucket per client that refills at a fixed rate up to a maximum capacity; each request spends a token. A full bucket permits a short burst, while sustained traffic is throttled to the refill rate. A fixed window counter simply counts requests in the current minute, which is cheaper but lets a client send twice the limit by straddling the boundary.

When a request is refused the server answers 429 Too Many Requests, and should say when to try again. The Retry-After header gives the wait in seconds or as a date; well-behaved APIs also report the limit, remaining quota and reset time in response headers.

Clients are expected to back off on 429. Retrying immediately after a fixed interval concentrates retries into synchronized waves right after the window resets, recreating the spike. Add random jitter to the delay so retries spread out, and honor the server-provided wait time when present. On the server side, apply the limit before expensive work like authentication callbacks or database reads, keep the counter in shared storage rather than per-process memory so limits hold across replicas, and reserve a tighter limit for unauthenticated traffic than for known API keys.
