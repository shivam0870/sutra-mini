#!/usr/bin/env bash
# Exercise the production app and actual worker processes without Docker.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -x "$PWD/.local/toolchains/go/bin/go" ]; then
  export PATH="$PWD/.local/toolchains/go/bin:$PATH"
fi
if [ -z "${SUTRA_PG_BIN:-}" ]; then
  if [ -d /opt/homebrew/opt/postgresql@17/bin ]; then
    SUTRA_PG_BIN=/opt/homebrew/opt/postgresql@17/bin
  else
    SUTRA_PG_BIN="$(pg_config --bindir)"
  fi
fi
for executable in go npm node curl; do
  command -v "$executable" >/dev/null || { echo "Install $executable before running native E2E." >&2; exit 1; }
done
for executable in initdb pg_ctl createdb psql; do
  [ -x "$SUTRA_PG_BIN/$executable" ] || { echo "PostgreSQL executable missing: $SUTRA_PG_BIN/$executable" >&2; exit 1; }
done

mkdir -p .local artifacts/native-e2e
run_dir="$(mktemp -d "$PWD/.local/native-e2e.XXXXXX")"
log_dir="$PWD/artifacts/native-e2e"
pids=()
pg_started=false
cleanup() {
  result=$?
  trap - EXIT
  # macOS Bash 3.2 treats an empty array as unbound under set -u.
  for pid in ${pids[@]+"${pids[@]}"}; do kill "$pid" 2>/dev/null || true; done
  for pid in ${pids[@]+"${pids[@]}"}; do wait "$pid" 2>/dev/null || true; done
  if [ "$pg_started" = true ]; then
    "$SUTRA_PG_BIN/pg_ctl" -D "$run_dir/pgdata" -m fast stop >> "$log_dir/postgres.log" 2>&1 || true
  fi
  # This path was created by mktemp above and contains only this run's data.
  rm -rf "$run_dir"
  if [ "$result" -ne 0 ]; then echo "Native E2E failed; logs are in $log_dir" >&2; fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pg_port="${SUTRA_E2E_PG_PORT:-55433}"
api_port="${SUTRA_E2E_API_PORT:-18080}"
mock_port="${SUTRA_E2E_MOCK_PORT:-18081}"
web_port="${SUTRA_E2E_WEB_PORT:-13000}"

# Ignore user database credentials: every run gets a new local cluster.
export DATABASE_URL="postgres://sutra@127.0.0.1:$pg_port/sutra?sslmode=disable"
export TEST_DATABASE_URL="$DATABASE_URL"
export API_ADDR="127.0.0.1:$api_port"
export MOCK_ADDR="127.0.0.1:$mock_port"
export ACCOUNTING_URL="http://127.0.0.1:$mock_port/api/vouchers"
export API_URL="http://127.0.0.1:$api_port"
export MOCK_MODE=success
export WORKER_CONCURRENCY=4 WORKER_MAX_ATTEMPTS=5
export WORKER_HTTP_TIMEOUT=1s WORKER_LEASE_DURATION=10s
export WORKER_POLL_INTERVAL=100ms WORKER_BASE_BACKOFF=100ms WORKER_MAX_BACKOFF=1s
export NEXT_TELEMETRY_DISABLED=1

"$SUTRA_PG_BIN/initdb" -D "$run_dir/pgdata" -A trust -U sutra --no-locale -E UTF8 > "$log_dir/initdb.log"
mkdir -p "$run_dir/socket"
"$SUTRA_PG_BIN/pg_ctl" -D "$run_dir/pgdata" -l "$log_dir/postgres.log" \
  -o "-p $pg_port -k $run_dir/socket -h 127.0.0.1" start
pg_started=true
"$SUTRA_PG_BIN/createdb" -h 127.0.0.1 -p "$pg_port" -U sutra sutra

go mod download
if [ "${SUTRA_SKIP_GO_TESTS:-0}" != 1 ]; then
  go vet ./...
  go test -race -count=1 -v ./... 2>&1 | tee "$log_dir/go-tests.log"
fi
for binary in api mock worker migrate; do go build -o "$run_dir/$binary" "./cmd/$binary"; done
"$run_dir/migrate"

start_service() {
  name="$1"; shift
  "$@" > "$log_dir/$name.log" 2>&1 &
  pids+=("$!")
}
wait_ready() {
  endpoint="$1"
  for attempt in $(seq 1 120); do
    if curl -fsS --max-time 2 "$endpoint" >/dev/null 2>&1; then return; fi
    sleep 0.5
  done
  echo "Service did not become ready: $endpoint" >&2
  return 1
}

start_service api "$run_dir/api"
start_service mock "$run_dir/mock"
wait_ready "$API_URL/health"
wait_ready "http://127.0.0.1:$mock_port/health"
start_service worker-1 "$run_dir/worker"
start_service worker-2 "$run_dir/worker"

"$SUTRA_PG_BIN/psql" "$DATABASE_URL" -v ON_ERROR_STOP=1 <<'SQL'
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

if [ "${SUTRA_SKIP_WEB_INSTALL:-0}" != 1 ]; then
  (cd web && npm ci && npx playwright install chromium)
fi
if [ "${SUTRA_SKIP_WEB_BUILD:-0}" != 1 ]; then
  (cd web && npm run typecheck && npm run build) 2>&1 | tee "$log_dir/web-build.log"
fi
mkdir -p web/.next/standalone/.next/static
cp -R web/.next/static/. web/.next/standalone/.next/static/
if [ -d web/public ]; then cp -R web/public web/.next/standalone/; fi
start_service web env HOSTNAME=127.0.0.1 PORT="$web_port" node web/.next/standalone/server.js
wait_ready "http://127.0.0.1:$web_port/sync"
(cd web && E2E_BASE_URL="http://127.0.0.1:$web_port" PLAYWRIGHT_HTML_OUTPUT_DIR="$log_dir/../playwright" npm run test:e2e -- --output="$log_dir/../test-results") 2>&1 | tee "$log_dir/browser-tests.log"
echo 'PASS: production Next.js and Chromium E2E against real PostgreSQL and two worker processes.'
if [ "${SUTRA_SKIP_GO_TESTS:-0}" != 1 ]; then echo 'PASS: Go race tests, including worker process crash/restart.'; fi
