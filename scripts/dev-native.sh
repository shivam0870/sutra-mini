#!/usr/bin/env bash
# Persistent local development stack. Only DATABASE_URL exported by the caller
# selects an external database; this script deliberately does not source .env.
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077

if [ -x "$PWD/.local/toolchains/go/bin/go" ]; then
  export PATH="$PWD/.local/toolchains/go/bin:$PATH"
fi
for executable in go npm node curl; do
  command -v "$executable" >/dev/null || { echo "Install $executable before running make dev-native." >&2; exit 1; }
done

dev_dir="$PWD/.local/dev"
log_dir="$dev_dir/logs"
lock_dir="$dev_dir/run.lock"
mkdir -p "$dev_dir/bin" "$log_dir"
if ! mkdir "$lock_dir" 2>/dev/null; then
  previous_pid="$(cat "$lock_dir/pid" 2>/dev/null || true)"
  if [[ "$previous_pid" =~ ^[0-9]+$ ]] && ! kill -0 "$previous_pid" 2>/dev/null; then
    rm -f "$lock_dir/pid"
    rmdir "$lock_dir"
    mkdir "$lock_dir"
  else
    echo "A native development runner already holds $lock_dir. Stop it before starting another." >&2
    exit 1
  fi
fi
echo "$$" > "$lock_dir/pid"

pids=()
service_names=()
pg_started=false
terminate_tree() {
  local parent_pid="$1" child_pid
  # Next's CLI starts a server child. Stop only descendants of processes that
  # this runner launched, including that child, rather than matching names.
  for child_pid in $(ps -axo pid=,ppid= | awk -v parent="$parent_pid" '$2 == parent { print $1 }'); do
    terminate_tree "$child_pid"
  done
  kill -TERM "$parent_pid" 2>/dev/null || true
}
cleanup() {
  result=$?
  trap - EXIT INT TERM
  for pid in ${pids[@]+"${pids[@]}"}; do terminate_tree "$pid"; done
  for pid in ${pids[@]+"${pids[@]}"}; do wait "$pid" 2>/dev/null || true; done
  if [ "$pg_started" = true ]; then
    "$SUTRA_PG_BIN/pg_ctl" -D "$dev_dir/pgdata" -m fast -w stop >> "$log_dir/postgres.log" 2>&1 || true
  fi
  rm -f "$lock_dir/pid"
  rmdir "$lock_dir" 2>/dev/null || true
  if [ "$result" -ne 0 ] && [ "$result" -ne 130 ]; then
    echo "Native development stopped with an error. Inspect $log_dir" >&2
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

pg_port="${SUTRA_DEV_PG_PORT:-55432}"
api_port="${SUTRA_DEV_API_PORT:-8080}"
mock_port="${SUTRA_DEV_MOCK_PORT:-8081}"
web_port="${SUTRA_DEV_WEB_PORT:-3000}"
for port in "$pg_port" "$api_port" "$mock_port" "$web_port"; do
  if ! [[ "$port" =~ ^[0-9]+$ ]] || [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
    echo "Ports must be integers between 1 and 65535." >&2
    exit 1
  fi
done

# Fail before migrations if another application already owns a service port.
for port in "$api_port" "$mock_port" "$web_port"; do
  node - "$port" <<'JS'
const net = require('node:net');
const port = Number(process.argv[2]);
const server = net.createServer();
server.on('error', () => {
  console.error(`Port ${port} is unavailable; stop its owner or set SUTRA_DEV_API_PORT, SUTRA_DEV_MOCK_PORT, or SUTRA_DEV_WEB_PORT.`);
  process.exitCode = 1;
});
server.listen(port, '127.0.0.1', () => server.close());
JS
done

if [ -z "${DATABASE_URL:-}" ]; then
  if [ -z "${SUTRA_PG_BIN:-}" ]; then
    if [ -d /opt/homebrew/opt/postgresql@17/bin ]; then
      SUTRA_PG_BIN=/opt/homebrew/opt/postgresql@17/bin
    elif [ -d /usr/local/opt/postgresql@17/bin ]; then
      SUTRA_PG_BIN=/usr/local/opt/postgresql@17/bin
    elif command -v pg_config >/dev/null; then
      SUTRA_PG_BIN="$(pg_config --bindir)"
    else
      echo "Install PostgreSQL (on macOS: brew install postgresql@17), set SUTRA_PG_BIN, or export DATABASE_URL." >&2
      exit 1
    fi
  fi
  for executable in initdb pg_ctl createdb psql; do
    [ -x "$SUTRA_PG_BIN/$executable" ] || { echo "PostgreSQL executable missing: $SUTRA_PG_BIN/$executable" >&2; exit 1; }
  done
  if [ ! -f "$dev_dir/pgdata/PG_VERSION" ]; then
    echo "Initializing the local development database..."
    "$SUTRA_PG_BIN/initdb" -D "$dev_dir/pgdata" -A trust -U sutra --no-locale -E UTF8 > "$log_dir/initdb.log" 2>&1
  fi
  if ! "$SUTRA_PG_BIN/pg_ctl" -D "$dev_dir/pgdata" status >/dev/null 2>&1; then
    # Disable Unix sockets and expose trust authentication only on loopback.
    "$SUTRA_PG_BIN/pg_ctl" -D "$dev_dir/pgdata" -l "$log_dir/postgres.log" \
      -o "-p $pg_port -k '' -h 127.0.0.1" -w start > "$log_dir/postgres-start.log" 2>&1
    pg_started=true
  fi
  if ! "$SUTRA_PG_BIN/psql" -h 127.0.0.1 -p "$pg_port" -U sutra -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='sutra'" > "$dev_dir/database-exists" 2> "$log_dir/database-check.log"; then
    echo "Cannot connect to local PostgreSQL; inspect $log_dir/database-check.log" >&2
    exit 1
  fi
  if [ "$(cat "$dev_dir/database-exists")" != 1 ]; then
    "$SUTRA_PG_BIN/createdb" -h 127.0.0.1 -p "$pg_port" -U sutra sutra > "$log_dir/createdb.log" 2>&1
  fi
  export DATABASE_URL="postgres://sutra@127.0.0.1:$pg_port/sutra?sslmode=disable"
  echo "Using persistent local PostgreSQL on 127.0.0.1:$pg_port."
else
  echo "Using the supplied DATABASE_URL. Applying migrations to that database."
fi

export API_ADDR="127.0.0.1:$api_port" MOCK_ADDR="127.0.0.1:$mock_port"
export API_URL="http://127.0.0.1:$api_port"
export ACCOUNTING_URL="http://127.0.0.1:$mock_port/api/vouchers"
export MOCK_MODE="${MOCK_MODE:-random}"
export NEXT_TELEMETRY_DISABLED=1

echo "Building API, mock accounting, workers, and migrations..."
for binary in api mock worker migrate; do
  go build -o "$dev_dir/bin/$binary" "./cmd/$binary" > "$log_dir/build-$binary.log" 2>&1
done
"$dev_dir/bin/migrate" > "$log_dir/migrate.log" 2>&1
if [ ! -d web/node_modules ]; then
  echo "Installing web dependencies from package-lock.json..."
  (cd web && npm ci) > "$log_dir/npm-install.log" 2>&1
fi

start_service() {
  local name="$1"
  shift
  "$@" > "$log_dir/$name.log" 2>&1 &
  pids+=("$!")
  service_names+=("$name")
}
check_processes() {
  local index
  for ((index=0; index<${#pids[@]}; index++)); do
    if ! kill -0 "${pids[$index]}" 2>/dev/null; then
      echo "${service_names[$index]} exited; inspect $log_dir/${service_names[$index]}.log" >&2
      return 1
    fi
  done
}
wait_ready() {
  local endpoint="$1" attempt
  for ((attempt=0; attempt<120; attempt++)); do
    check_processes || return 1
    if curl -fsS --max-time 2 "$endpoint" >/dev/null 2>&1; then
      check_processes
      return
    fi
    sleep 0.5
  done
  echo "Service did not become ready: $endpoint; inspect $log_dir" >&2
  return 1
}

start_service api "$dev_dir/bin/api"
start_service mock "$dev_dir/bin/mock"
wait_ready "$API_URL/health"
wait_ready "http://127.0.0.1:$mock_port/health"
start_service worker-1 "$dev_dir/bin/worker"
start_service worker-2 "$dev_dir/bin/worker"
start_service web node "$PWD/web/node_modules/next/dist/bin/next" dev "$PWD/web" --hostname 127.0.0.1 --port "$web_port"
wait_ready "http://127.0.0.1:$web_port/sync"
echo "Ready: http://127.0.0.1:$web_port/sync"
echo "Two workers are running. Mock mode: $MOCK_MODE. Logs: $log_dir"
echo 'Press Ctrl+C to stop this stack. Database records are preserved.'
while true; do
  check_processes
  sleep 2
done
