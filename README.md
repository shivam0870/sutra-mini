# Sutra Mini

A small Go + PostgreSQL/Neon + Next.js application for receiving job-work and reliably posting its accounting vouchers. Open `/sync` to record receipts, see outstanding quantities, and monitor or retry vouchers.

## Start locally without Docker

From the cloned repository, with the prerequisites below installed:

```sh
make dev-native
```

Open **http://127.0.0.1:3000/sync**. Keep the terminal open; **Ctrl+C** stops the stack. The launcher starts PostgreSQL, applies migrations and seed data, builds the Go services, runs the API, mock and **two workers**, and starts Next.js. Records survive restarts in `.local/dev/pgdata`; logs are in `.local/dev/logs`. The default mock has the failures required by the brief. For a predictable successful demonstration, use `MOCK_MODE=success make dev-native`.

On another machine, first clone the repository and install Go 1.24+, Node 20.9+, npm, and PostgreSQL 14+ server binaries. For example, with Homebrew already installed on macOS:

```sh
brew install go node postgresql@17
make dev-native
```

The launcher recognizes Homebrew PostgreSQL 17, or accepts `SUTRA_PG_BIN=/path/to/postgres/bin`. It installs locked web dependencies when `web/node_modules` is absent. It recognizes this workspace's Go installation under `.local/toolchains/go/bin`; that installation is ignored by Git. Native local ports default to `55432`, `8080`, `8081`, and `3000`; use `SUTRA_DEV_PG_PORT`, `SUTRA_DEV_API_PORT`, `SUTRA_DEV_MOCK_PORT`, or `SUTRA_DEV_WEB_PORT` to override them.

See [AGENT_NOTES.md](AGENT_NOTES.md) for AI-use disclosure and design decisions, and [TEST_RESULTS.md](TEST_RESULTS.md) for executed verification and its limits.

## Run with Docker

Install Docker Desktop (or Docker Engine with Compose 2.24.4+), then:

```sh
cp .env.example .env
make up
```

Open **http://localhost:3000/sync**. `make up` builds the application, applies migrations, seeds five SKUs, two job-workers and four open issues, and runs **two worker instances**. Initial voucher lists are empty; submit a receipt in the page to create one. The first build needs internet access to download images and packages.

```sh
make down                     # stop; retain local database
bash scripts/compose.sh logs -f api worker mock
```

The browser uses Next.js `/api/*` rewrites to reach Go; no cross-origin browser configuration is needed. Service endpoints on the host are `localhost:8080` (Go) and `localhost:8081` (mock). Host ports bind to loopback.

### Use Neon

Create a Neon project/database and put its PostgreSQL connection string in `.env`:

```dotenv
DATABASE_URL='postgresql://USER:PASSWORD@YOUR-HOST/DB?sslmode=require'
```

Then run `make up`. API, migration, worker and mock processes all receive that URL. Migrations use PostgreSQL transactions and transaction-scoped advisory locks, which work with Neon's transaction pooling. The local database container also starts for convenience but receives no application data when a Neon URL is configured. Use a dedicated Neon database for this exercise; the mock's durable ledger lives there too, in a separate table. A direct Neon connection is simplest for tests that create isolated schemas.

For the native launcher, export `DATABASE_URL` in the shell instead. To load the Neon URL you saved in `.env`, use:

```sh
set -a
. ./.env
set +a
make dev-native
```

The native launcher deliberately does **not** load `.env` automatically: the example file's `db` hostname is for Docker. With an exported `DATABASE_URL`, it uses that database and skips local PostgreSQL startup. Migrations and seeds run against the supplied database, so use a dedicated case-study database. To return to the persistent local database, stop the launcher, run `unset DATABASE_URL`, and run `make dev-native` again.

**Hosted Neon remains unverified.** The successful E2E run used real local PostgreSQL 17. No Neon project or credentials have been provisioned by this repository. `.env` is ignored by Git and Docker build contexts. Run the receipt, replay and Retry demonstrations against Neon before describing the hosted path as tested.

### Run individual services manually

Prerequisites: Go 1.24+, Node 20.9+ (Node 24 recommended), PostgreSQL 14+, npm. Docker is optional with a running PostgreSQL/Neon database.

```sh
cp .env.example .env
# For a native local database change the DATABASE_URL hostname from db to localhost.
# Export your chosen DATABASE_URL in EACH Go terminal.
set -a; . ./.env; set +a
go mod download
go run ./cmd/migrate
go run ./cmd/api
```

In separate terminals, with the same environment:

```sh
go run ./cmd/mock
go run ./cmd/worker
go run ./cmd/worker            # optional second instance, another terminal
```

Start the frontend:

```sh
cd web
npm ci
API_URL=http://localhost:8080 npm run dev
```

Go binaries do not implicitly read `.env`; the shell commands above export it. Next's API destination is compiled into a production build, so rebuild the web image after changing it. The Docker configuration passes its internal API address automatically. Direct npm dependencies are pinned and `web/package-lock.json` locks the installed dependency tree; both native setup and Docker use `npm ci`.

## Flow and guarantees

```mermaid
sequenceDiagram
    actor User
    participant UI as Next.js /sync
    participant API as Go API
    participant DB as PostgreSQL / Neon
    participant W as Worker instances
    participant A as Mock accounting
    User->>UI: Receive pieces
    UI->>API: POST receipt + Idempotency-Key
    API->>DB: BEGIN, lock key, check saved request
    API->>DB: Insert receipt; trigger reserves remaining quantity
    API->>DB: Insert pending voucher and save response
    API->>DB: COMMIT
    API-->>UI: 201 receipt + voucher
    W->>DB: Claim due row with SKIP LOCKED and a lease token
    W->>A: POST with stable client_ref
    A->>DB: Persist one accounting entry per client_ref
    A-->>W: 201 or 409 with original external_ref
    W->>DB: Mark pushed only if lease token still matches
    UI->>API: Poll status every 2 seconds
```

- **No over-receiving:** migration `003` adds an atomic conditional update of the issue's received counter from a receipt insertion trigger. PostgreSQL locks the issue row, rechecks capacity after competing updates, and rejects overshoot. A database check enforces `0 <= received_qty <= qty`; the trigger applies even to direct SQL inserts. Receipts are append-only except for the saved response. A failed voucher insert rolls back the receipt and capacity reservation too.
- **Idempotent receipts:** a transaction-scoped advisory lock serializes an idempotency key; a unique constraint persists its uniqueness. A fingerprint rejects reusing that key for a different payload with `409`. Both the first response and replays return `201` and the same stored JSON, including original IDs and status. Replays have `Idempotency-Replayed: true`. GET `/vouchers` is the source of current delivery status.
- **Durable queue:** the voucher row is the outbox. A receipt is never committed without its voucher by the API. No in-memory queue is required.
- **Multiple workers:** each available processing slot claims one due row using `FOR UPDATE SKIP LOCKED`. The row gets a lease, a random token, and an incremented attempt count. Every completion update checks that token; an expired worker cannot overwrite a newer claim.
- **Crash recovery:** after a crash, the lease expires and another worker sends the same `client_ref`. If accounting already accepted it, its durable unique constraint returns `409` with the original `external_ref`; the worker treats that as confirmation. Requests may be repeated; accounting entries are created once. This guarantee relies on the accounting service retaining its deduplication record, not on PostgreSQL magically sharing a transaction with HTTP.
- **Bounded failure:** each HTTP request has a one-second timeout; four concurrent slots prevent a five-second response from blocking all vouchers. Observed failures retry after 1, 2, 4, 8 seconds, capped at 30 seconds; the fifth observed failed attempt becomes `failed` with a reason. A hard crash can leave an attempt with an unknown outcome; recovery may exceed the configured attempt count to reconcile it. A further observed error then fails it. Manual retry resets the attempt budget and preserves `client_ref`.

The mock chooses 20% HTTP 500, 10% five-second responses, 70% immediate success for new references. It commits an accepted entry **before** delaying its response; this deliberately exposes the uncertain-outcome crash case. Duplicates always return `409`, before failure injection. `MOCK_MODE=success|fail|hang|random` provides deterministic demonstrations without a test-control HTTP endpoint.

Money is an integer number of paise. The brief supplies no rate, so this exercise explicitly assumes **₹25 (2,500 paise) per received piece**. Receipts use whole pieces. Authentication, production permissions, pagination, metrics infrastructure, and accounting reversal workflows are outside this small case study. The optional `/ask` stretch is not implemented.

## API examples

```sh
curl -sS http://localhost:8080/issues/1

curl -i http://localhost:8080/jobwork/receipts \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: receipt-demo-001' \
  -d '{"issue_id":1,"qty":5,"received_on":"2026-10-10"}'

# Repeat the exact command: same receipt/voucher, no additional quantity.
# Reuse that key with qty=6: HTTP 409.
curl -sS 'http://localhost:8080/vouchers?status=failed'
curl -i -X POST http://localhost:8080/vouchers/1/retry
```

| Endpoint | Behavior |
| --- | --- |
| `GET /health` | Database reachability |
| `GET /issues`, `GET /issues/{id}` | Issued, received, outstanding, SKU and party |
| `POST /jobwork/receipts` | Requires `Idempotency-Key`; saves receipt + voucher atomically |
| `GET /vouchers?status=pending\|pushed\|failed` | Optional status filter |
| `POST /vouchers/{id}/retry` | Failed → pending; rejects pending/pushed with `409` |
| Mock `POST /api/vouchers` | `{client_ref,type,amount_paise}` → `{external_ref}` |

## Tests

The complete suite can run natively, without Docker:

```sh
make e2e-native
```

This requires Go, Node/npm, and PostgreSQL binaries (including `initdb` and `pg_ctl`). The script recognizes the workspace Go installation at `.local/toolchains/go/bin` and Homebrew PostgreSQL 17, or accepts `SUTRA_PG_BIN=/path/to/postgres/bin`. It creates a fresh local database cluster, runs Go tests with the race detector, installs locked web dependencies and Chromium, builds the **production** Next.js app, starts **two real worker processes**, and runs all browser tests. It ignores `.env` and removes its own database/processes on exit. Default test ports are 55433, 18080, 18081 and 13000; override them with `SUTRA_E2E_PG_PORT`, `SUTRA_E2E_API_PORT`, `SUTRA_E2E_MOCK_PORT` and `SUTRA_E2E_WEB_PORT` if occupied. Logs are in `artifacts/native-e2e/`, with the browser report in `artifacts/playwright/` and videos/screenshots in `artifacts/test-results/`.

The Docker/CI route is:

```sh
make e2e
```

This builds a **separate disposable Docker project**, ignores `.env`/Neon credentials, publishes no host ports, runs Go tests with the race detector against real PostgreSQL, starts the full app with two workers, and runs Chromium against `/sync`. It seeds one failed voucher for the browser Retry test, then destroys only its own database/containers. Browser reports, videos and screenshots go in `artifacts/`; failure service logs go in `artifacts/e2e-services.log`. CI runs this same command.

The suite includes:

- Ten concurrent receipts competing for insufficient capacity; exact accepted/rejected counts and database totals asserted.
- Concurrent and subsequent idempotency replay, changed payload conflict, replay after a voucher has progressed, and forced voucher failure proving receipt/counter rollback.
- Direct-SQL quantity guards, stale worker token rejection, twenty vouchers handled by two instances, timeout behavior, retry exhaustion and head-of-line blocking checks.
- A **real worker process killed during the mock's five-second response**, then two replacement processes recovering the original external reference with exactly one ledger entry.
- Browser receipt submission → pushed, duplicate replay/conflict, overshoot rejection and failed → Retry → pushed.

Backend only:

```sh
make test
# Or, with Go and a PostgreSQL URL that permits CREATE SCHEMA:
TEST_DATABASE_URL='postgres://USER:PASSWORD@localhost:5432/sutra_test?sslmode=disable' make test-go
```

Each Go database test creates and drops its own schema. Running `go test ./...` without `TEST_DATABASE_URL` runs unit tests but **skips** database/process tests. A configured but inaccessible test database fails the tests. `make test-go` refuses to run without that variable.

**Verified on 2026-10-10:** Go compilation, `go vet`, **23 Go tests plus 8 subtests with the race detector and no skips**, TypeScript typecheck, Next.js production build, and **all 4 Chromium E2E tests** passed against real PostgreSQL 17. This includes the actual worker SIGKILL/restart scenario and a successful browser Retry. See [TEST_RESULTS.md](TEST_RESULTS.md) for evidence and [AGENT_NOTES.md](AGENT_NOTES.md) for the issue caught during the browser run. Docker Compose configuration passed validation; container execution remains unverified because the optional Colima VM image download failed with a remote connection reset. Neon itself has not been provisioned or tested.

## Layout and submission

`cmd/{api,worker,mock,migrate}` are independent processes. `internal/api` handles HTTP/transactions; `internal/syncworker` handles delivery; `internal/mockaccounting` is the unreliable external service. `migrations` embeds versioned SQL and seed data. `integration` exercises real process crashes. `web` contains the single Next.js page and Playwright tests.

[AGENT_NOTES.md](AGENT_NOTES.md) records how agents were used and what was caught. The submission walkthrough should demonstrate a retry succeeding and a duplicate being rejected. Hosted Neon remains unverified. Local browser test recordings are generated under `artifacts/test-results/`; personal preparation notes, correspondence, and the original invitation are excluded from the repository.
