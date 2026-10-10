#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

command -v docker >/dev/null || { echo 'Docker with Compose 2.24.4+ is required.' >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo 'Start Docker, then run make e2e again.' >&2; exit 1; }
project="sutra-e2e-$$"
artifacts_dir="${E2E_ARTIFACTS_DIR:-./artifacts}"
mkdir -p "$artifacts_dir/playwright" "$artifacts_dir/test-results"
compose() {
  # This suite always uses its own disposable local database, never Neon/.env.
  env -u DATABASE_URL -u TEST_DATABASE_URL -u WORKER_HTTP_TIMEOUT \
    -u WORKER_LEASE_DURATION -u WORKER_CONCURRENCY -u WORKER_MAX_ATTEMPTS \
    -u WORKER_BASE_BACKOFF -u WORKER_MAX_BACKOFF -u WORKER_POLL_INTERVAL \
    MOCK_MODE=success bash scripts/compose.sh --env-file /dev/null --project-name "$project" \
    -f compose.yaml -f compose.e2e.yaml --profile test "$@"
}
cleanup() {
  result=$?
  if [ "$result" -ne 0 ]; then
    compose logs --no-color > "$artifacts_dir/e2e-services.log" 2>&1 || true
    echo "E2E failed. Service logs: $artifacts_dir/e2e-services.log" >&2
  fi
  compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  exit "$result"
}
trap cleanup EXIT

compose up -d --wait db
compose run --build --rm test
compose up --build -d --wait --scale worker=2 api mock worker web

# Seed one terminal failure for the browser Retry flow. Only this disposable
# test project receives fixture SQL; production has no test-control endpoint.
compose exec -T db psql -v ON_ERROR_STOP=1 -U sutra -d sutra <<'SQL'
BEGIN;
WITH receipt AS (
  INSERT INTO jobwork_receipts(issue_id,qty,received_on,idempotency_key,request_fingerprint,response_json)
  VALUES(4,1,CURRENT_DATE,'browser-retry-fixture','fixture','{}'::jsonb)
  RETURNING id
)
INSERT INTO vouchers(type,source_id,amount_paise,status,attempts,last_error,client_ref)
SELECT 'jobwork_receipt',id,2500,'failed',5,'E2E accounting outage','browser-retry-fixture' FROM receipt;
COMMIT;
SQL

compose run --build --rm browser
echo 'Go/PostgreSQL/process E2E and Chromium browser E2E completed successfully.'
