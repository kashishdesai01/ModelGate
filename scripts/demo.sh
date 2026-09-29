#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${COMPOSE:-}" ]]; then
  if command -v docker-compose >/dev/null 2>&1; then COMPOSE=docker-compose; else COMPOSE="docker compose"; fi
fi
API_KEY=${MODEL_GATE_API_KEY:-mg_demo_local_only_change_me}

$COMPOSE up -d --build
for _ in $(seq 1 60); do
  if curl --fail --silent http://localhost:18080/readyz >/dev/null; then break; fi
  sleep 1
done
curl --fail --silent http://localhost:18080/readyz >/dev/null || { echo "gateway did not become ready" >&2; exit 1; }

printf '\n== sklearn prediction ==\n'
curl --fail --silent --show-error -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
  -d '{"strategy":"sklearn","text":"The kitchen sink pipe is leaking and needs help this week"}' \
  http://localhost:18080/v1/predictions
printf '\n\n== hybrid prediction and cache hit ==\n'
for _ in 1 2; do
  curl --fail --silent --show-error -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
    -d '{"strategy":"hybrid","text":"An unusual home problem needs attention soon"}' \
    http://localhost:18080/v1/predictions
  printf '\n'
done
printf '\n== usage ==\n'
curl --fail --silent --show-error -H "Authorization: Bearer $API_KEY" http://localhost:18080/v1/usage
printf '\n\n== passing evaluation ==\n'
go run ./cmd/modelgate eval --dataset datasets/golden.jsonl --classifier-url http://localhost:18000 --baseline baseline-v1 --candidate candidate-v2 --output artifacts/eval-pass
printf '\n== known-bad evaluation (expected rejection) ==\n'
if go run ./cmd/modelgate eval --dataset datasets/golden.jsonl --classifier-url http://localhost:18000 --baseline baseline-v1 --candidate known-bad-v1 --output artifacts/eval-known-bad; then
  echo "known-bad candidate unexpectedly passed" >&2
  exit 1
fi
.venv/bin/python scripts/assert_known_bad_report.py artifacts/eval-known-bad/report.json
echo "known-bad candidate correctly rejected only by the critical slice gate"
