# Security policy

ModelGate is a portfolio demonstration, not an internet-ready hosted service.

## Supported deployment boundary

The supported demo runs on a trusted development machine through Docker Compose. Published gateway and classifier ports bind to `127.0.0.1`; the included API key and database credentials are intentionally local-only examples and must be replaced before any shared deployment.

## Security properties

- API keys are stored as SHA-256 hashes. This is appropriate only because keys must be generated with high entropy; it is not password hashing.
- SQL uses parameterized pgx statements.
- Backend URLs are server-owned configuration, never request input.
- Request and response bodies are not written to Postgres or structured logs.
- Redis cache keys are HMAC-derived and isolate user, strategy, model identities, routing policy, thresholds, and request text.
- The public gateway and LLM mock bound request bytes, and Go adapters bound upstream response bytes. The loopback-only FastAPI classifier enforces text length after JSON parsing rather than a transport-level body cap.
- Redirects from model backends are rejected.
- Containers run as non-root users.

## Known limitation

Postgres cannot atomically commit with an external model provider. If the gateway crashes after a budget reservation and before reconciliation, that reservation remains pending. ModelGate Lite documents this instead of claiming exactly-once accounting. A production system would add provider-usage reconciliation and safe stale-reservation recovery.

Report vulnerabilities privately to the repository owner. Do not include secrets or exploit data in a public issue.
