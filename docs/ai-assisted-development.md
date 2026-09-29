# AI-assisted development notes

ModelGate was built with AI assistance, but generated suggestions were treated as hypotheses and verified with tests, runtime probes, and dependency checks. These are concrete examples from this implementation.

## 1. Demo seeding silently destroyed the state it was supposed to demonstrate

The first Compose seed implementation reset monthly spend and reserved amounts and deleted the user's call history on every startup. Unit and gateway-restart checks passed because they never recreated the seed container. An independent AI review traced the startup path and reproduced spend falling from 100 micro-USD to zero after `down` followed by `up` without deleting the Postgres volume.

**Correction:** changed seeding into idempotent provisioning: it can update the configured limit and ensure the current period exists, but it cannot reset spend, reserved funds, or audit rows. Destructive reset remains an explicit operator action via volume deletion.

**Validation:** the Postgres integration test spends funds, reseeds, and checks that both usage and the call row survive. `scripts/verify.sh` now recreates the full Compose stack without deleting volumes and compares usage plus audit-row counts before and after.

## 2. Cancellation propagated too far and leaked reserved budget

The first handler reused `request.Context()` for budget reconciliation. That looked consistent with normal cancellation propagation, but after a client disconnect the context was already canceled, so the cleanup transaction could not release the reservation. Repeated abandoned requests could exhaust a budget without any completed inference.

**Correction:** backend work still stops with the client context, while post-reservation reconciliation uses `context.WithoutCancel` plus a strict two-second timeout. The period recorded with the reservation remains authoritative even if the request crosses a month boundary.

**Validation:** the handler regression test cancels the live request context inside prediction and asserts that reconciliation receives a usable cleanup context. The Postgres suite independently verifies cross-period reconciliation releases the original period's reservation.

## 3. Dependency selection was challenged instead of accepted

The first suggested `go-redis` version (`v9.7.0`) triggered live OSV advisory warnings during installation.

**Correction:** inspected the advisory's fixed-version ranges and moved to `v9.7.3`, the patched release on the same minor line. A later `govulncheck` found reachable issues in `pgx`, `x/text`, and the Go 1.26.3 standard library, so the project moved to `pgx v5.9.2`, `x/text v0.39.0`, and Go 1.26.6 or newer. Python auditing also forced a FastAPI/Starlette upgrade and replacement of deprecated `httpx` test support with `httpx2`.

**Validation:** `govulncheck ./...` reports no reachable vulnerabilities, `pip-audit --skip-editable` reports no known vulnerabilities, and both language test suites run without warnings.

## Verification policy

- Tests are written before core behavior and must fail for the expected reason.
- Concurrency claims require a real Postgres integration test.
- The known-bad model must be rejected by an actual nonzero eval command.
- Benchmark numbers are published only from saved raw output.
- Model reproducibility requires byte-identical cross-process artifacts, not merely identical predictions.
- AI-generated output is never accepted solely because it compiles.
