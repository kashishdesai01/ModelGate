#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${COMPOSE:-}" ]]; then
  if command -v docker-compose >/dev/null 2>&1; then COMPOSE=docker-compose; else COMPOSE="docker compose"; fi
fi
PROJECT="modelgate-verify-$RANDOM-$$"
SCOPED_COMPOSE="$COMPOSE -p $PROJECT"
BEFORE=$(mktemp)
AFTER=$(mktemp)
cleanup() {
  $SCOPED_COMPOSE down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -f "$BEFORE" "$AFTER"
}
trap cleanup EXIT

make test
make lint
COMPOSE="$SCOPED_COMPOSE" ./scripts/demo.sh
INVALID_STATUS=$(curl --silent --output /dev/null --write-out '%{http_code}' \
  -H 'Authorization: Bearer mg_demo_local_only_change_me' \
  -H 'Content-Type: application/json' \
  -H 'X-Request-ID: verify-invalid-llm' \
  -d '{"strategy":"llm","text":"[mock:invalid] validate accounting"}' \
  http://localhost:18080/v1/predictions)
test "$INVALID_STATUS" = "502"
INVALID_AUDIT=$($SCOPED_COMPOSE exec -T postgres psql -U modelgate -d modelgate -Atc \
  "SELECT state || ':' || actual_cost_microusd || ':' || model_kind FROM inference_calls WHERE request_id='verify-invalid-llm'")
test "$INVALID_AUDIT" = "failed:1000:llm_mock"
.venv/bin/python scripts/load_test.py \
  --url http://localhost:18080/v1/predictions \
  --api-key mg_demo_local_only_change_me \
  --requests 100 --concurrency 10 \
  --output artifacts/load-test-latest.json

curl --fail --silent -H 'Authorization: Bearer mg_demo_local_only_change_me' \
  http://localhost:18080/v1/usage > "$BEFORE"
BEFORE_CALLS=$($SCOPED_COMPOSE exec -T postgres psql -U modelgate -d modelgate -Atc 'SELECT count(*) FROM inference_calls')
$SCOPED_COMPOSE down --remove-orphans
$SCOPED_COMPOSE up -d
for _ in $(seq 1 60); do
  if curl --fail --silent http://localhost:18080/readyz >/dev/null; then break; fi
  sleep 1
done
curl --fail --silent --show-error http://localhost:18080/readyz >/dev/null
curl --fail --silent -H 'Authorization: Bearer mg_demo_local_only_change_me' \
  http://localhost:18080/v1/usage > "$AFTER"
AFTER_CALLS=$($SCOPED_COMPOSE exec -T postgres psql -U modelgate -d modelgate -Atc 'SELECT count(*) FROM inference_calls')
cmp "$BEFORE" "$AFTER"
test "$BEFORE_CALLS" = "$AFTER_CALLS"
printf 'fresh Compose verification passed; usage and audit state persisted across stack recreation\n'
