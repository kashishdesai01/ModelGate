# ModelGate

ModelGate is a compact Go AI/ML serving and evaluation gateway. It serves a versioned scikit-learn classifier behind FastAPI, supports deterministic structured-LLM fallback, enforces per-user rate and monetary budgets, caches successful responses, records payload-free call metadata in Postgres, and blocks model regressions through a Go evaluation CLI.

The project uses deterministic synthetic home-services data and is not affiliated with Thumbtack.

## Why this exists

A model release should answer more than “does the endpoint respond?” ModelGate demonstrates a small production-shaped loop:

1. serve a versioned traditional ML model;
2. route low-confidence predictions through a bounded fallback;
3. control request rate and spend before calling a backend;
4. record the exact model and policy used; and
5. compare a candidate to a frozen baseline before release.

## Architecture

```text
Client
  │  Bearer key + JSON
  ▼
Go ModelGate :8080
  ├─ strict request validation and request IDs
  ├─ Redis token bucket (fail closed)
  ├─ Redis user/model/policy-scoped response cache
  ├─ Postgres budget reservation/reconciliation
  ├─ bounded sklearn and LLM backend concurrency
  └─ structured logs + /metrics
       │                         │
       ▼                         ▼
FastAPI/scikit-learn :8000   deterministic LLM mock :8081
  baseline-v1
  candidate-v2
  known-bad-v1

Go eval CLI ── paired predictions ──> report.json + report.md + case_results.jsonl
```

## Prediction contract

```bash
curl -sS http://localhost:18080/v1/predictions \
  -H 'Authorization: Bearer mg_demo_local_only_change_me' \
  -H 'Content-Type: application/json' \
  -d '{"strategy":"hybrid","text":"The breaker is sparking and smells like smoke"}'
```

Strategies:

- `sklearn`: always use the configured traditional model.
- `llm`: always use the structured-output mock.
- `hybrid`: use sklearn first; fall back when category confidence is below 0.60 or urgency confidence is below 0.65. An sklearn emergency is preserved. Invalid, timed-out, or failed fallback keeps the validated sklearn result and records a fallback error.

## One-command demo

Prerequisites: Go 1.26.6+, Python 3.12+, Docker, and either `docker-compose` or Docker Compose v2.

```bash
make setup
make demo
```

The demo builds and starts the stack on host loopback only, exercises prediction and caching, displays budget usage, runs a passing evaluation, and proves that the known-bad fixture exits nonzero.

Stop services without deleting Postgres data:

```bash
make down
```

Delete local state:

```bash
docker-compose down --volumes --remove-orphans
```

## Development commands

```bash
make setup        # create .venv and install pinned direct Python dependencies
make generate     # regenerate datasets and three model bundles
make test         # Go and Python tests
make test-race    # Go race detector
make lint         # gofmt, go vet, Ruff, and mypy
make build        # binaries and container images
make eval         # passing baseline vs candidate gate
make eval-known-bad # expected nonzero gate, wrapped as a successful assertion
make load-test    # save raw JSON benchmark output
```

The checked-in load-test workflow sends 100 unique, uncached sklearn requests at concurrency 10 and exits nonzero unless every request succeeds. The latest verified fresh-stack Apple Silicon/Colima run completed 100/100 requests with HTTP 200 at 110.15 requests/second (p50 26.89 ms, p95 365.05 ms); its raw output is checked in at `artifacts/load-test-latest.json`. This local synthetic result is evidence that the bounded path runs under concurrency, not a production SLO.

Run real Postgres and Redis integration tests:

```bash
MODEL_GATE_TEST_DATABASE_URL='postgres://modelgate:modelgate@localhost:5432/modelgate_test?sslmode=disable' \
MODEL_GATE_TEST_REDIS_ADDR='localhost:6379' \
  go test -race ./... -count=1
```

## Evaluation policy

The checked-in golden set contains 180 cases, including 36 typo-heavy emergencies. The CLI uses a bounded goroutine worker pool and calculates:

- category macro-F1;
- urgency macro-F1;
- emergency recall;
- paired deltas for every declared slice; and
- hard-gated typo-emergency slice regression.

Reports record the exact golden-set SHA-256 and verify both local model artifact digests against their manifests before sending any evaluation request. The supported Compose workflow builds and serves those same artifacts; when evaluating an independently deployed remote classifier, operators must separately attest that its reported version maps to the same bytes.

Model generation is byte-reproducible across separate Python processes and the supported ARM64/x86_64 build environments. Training removes scikit-learn's process-local stop-word object-ID cache and rounds solver coefficients below the observed BLAS noise floor before serialization; regression tests, a cross-architecture probe, and the CI clean-diff check protect the checked-in artifacts.

Verified controlled-benchmark results:

| Candidate | Category macro-F1 | Urgency macro-F1 | Emergency recall | Gate |
|---|---:|---:|---:|---|
| `candidate-v2` | 1.0000 | 1.0000 | 1.0000 | PASS |
| `known-bad-v1` | 1.0000 | 0.9895 | 0.9767 | FAIL |

The known-bad candidate selectively corrupts typo-emergency labels in its training data. Its aggregate urgency macro-F1 and emergency recall remain inside the declared release tolerances, but `noise=typo_emergency` emergency recall drops by 0.0556 across 36 cases. CI asserts that the critical slice is the only rejection reason. These synthetic results prove the gate mechanics, not real-world model quality.

## Budget correctness

For every cache miss, the gateway:

1. starts a Postgres transaction;
2. locks the user’s current budget row with `SELECT ... FOR UPDATE`;
3. checks `limit - spent - reserved`;
4. inserts a pending call with an immutable budget period and reserves the maximum strategy cost;
5. calls the backend outside the transaction; and
6. reconciles reserved versus actual cost in a second short transaction.

The integration test launches 20 concurrent reservations against a budget that permits five and asserts exactly five succeed. Additional integration tests prove idempotent reseeding, first-request monthly rollover, and cross-month reconciliation. Redis is never the source of truth for money.

## Failure behavior

| Failure | Behavior |
|---|---|
| Redis unavailable | Request fails closed before a backend call |
| Budget exhausted | `402 budget_exceeded`; no backend call |
| Rate exhausted | `429` with `Retry-After`; no backend call |
| Backend deadline | `504`; reservation reconciled as failed |
| Client disconnect after reservation | reconciliation uses a separate two-second cleanup context |
| Direct backend returns semantically invalid output | `502`; consumed backend cost and provenance are still reconciled |
| Invalid LLM output | Valid sklearn prediction retained with `fallback_error` |
| Audit finalization failure | Success is not reported and response is not cached |
| Cache unavailable | Cache is bypassed; request can continue |

`/readyz` covers the authoritative Postgres and Redis control-plane dependencies. Model backends are checked on each bounded request rather than included in readiness, so a partial backend outage can leave the gateway ready while the affected strategy returns an upstream error.

## Privacy and security

- Postgres stores no model input or output payloads.
- Logs contain no API keys, prompts, or model responses.
- Cache keys are HMAC-derived from user, strategy, request text, configured model identities, thresholds, and routing-policy version.
- API keys are stored only as hashes.
- Backends are static configuration, preventing user-controlled SSRF.
- Public gateway and LLM-mock bodies, server/upstream timeouts, queues, and concurrency are bounded. The loopback-only FastAPI classifier validates text length after JSON parsing.
- Containers run as non-root.

See [SECURITY.md](SECURITY.md) for the deployment boundary and known crash-accounting limitation.

## Repository map

```text
cmd/modelgate/             gateway, seed, and eval commands
cmd/llm-mock/              deterministic structured-output backend
internal/api/              HTTP handlers and metrics
internal/backend/          bounded HTTP model adapters
internal/control/          Redis rate limit and cache
internal/eval/             paired metrics, slice gates, reports, worker pool
internal/gateway/          prediction orchestration and fallback policy
internal/store/            Postgres auth, call log, and budget transactions
services/classifier/       FastAPI service, generator, training, tests
artifacts/                 checked evaluation evidence
docs/                      AI-assisted development evidence
```

## Deliberate scope cuts

ModelGate Lite does not include Kubernetes, Kafka, a model registry, OTel/Jaeger/Grafana, automatic promotion, a real paid LLM provider, generic chat, or LLM-as-judge. Those features would increase surface area without strengthening the core evidence: Go concurrency, ML serving, database correctness, evaluation tooling, CI gating, and defensible failure semantics.
