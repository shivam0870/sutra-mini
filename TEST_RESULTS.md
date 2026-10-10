# Executed test results — 10 October 2026

The full E2E suite ran locally against real PostgreSQL and the production Next.js build. An additional native launcher check used Next.js development mode. The API, database and accounting responses were not stubbed in browser tests.

| Check | Result |
| --- | --- |
| Go 1.26.9 dependency download, formatting and compilation | Passed |
| `go vet ./...` | Passed |
| PostgreSQL 17.11 migrations and seed data | Passed |
| `go test -race -count=1 -v ./...` | 23 tests + 8 subtests passed; zero skips/races |
| TypeScript `npm run typecheck` | Passed |
| Next.js 16.4.0 production build | Passed |
| Fresh frontend copy without `.next` or existing dependencies | `npm ci`, typecheck and production build passed |
| Playwright 1.64 / Chromium browser E2E | All 4 passed |
| `make dev-native` startup, persistence and shutdown | Passed; API, mock, two workers and UI started; records survived restart |
| Native launcher browser/API demonstration | Retry failed → pushed; exact replay 201; conflicting replay and accounting duplicate 409; original external reference preserved |
| Supplied `DATABASE_URL` launcher path | Passed against a supplied local PostgreSQL URL without using local PostgreSQL tools; hosted Neon remains untested |
| Launcher duplicate-run guard and invalid-port cleanup | Passed; no existing service interrupted and lock removed on failure |
| Native E2E early-failure cleanup on macOS Bash 3.2 | Passed after fixing empty-array handling; actual PostgreSQL startup failure preserved exit 1 and removed the temporary database directory |
| `go mod verify` | All modules verified |
| `npm audit --omit=dev` | Zero reported production dependency vulnerabilities |
| Docker Compose configuration | Passed |
| Docker container execution | Not run: VM image transfer reset by upstream host |
| Hosted Neon | Not configured/tested |

The process tests killed a real worker after accounting persisted a voucher during its five-second delayed response, started two replacement workers, and verified the original reference and exactly one accounting entry. Other backend checks cover concurrent capacity, concurrent idempotency, exact replay, atomic rollback, token fencing, two workers, HTTP timeouts and exhausted retries.

The four browser tests exercised receipt submission through to accounting confirmation, duplicate replay/conflicting reuse, a visible overshoot rejection without database changes, and a failed voucher's Retry button through to pushed status. The first run exposed an ambiguous test selector matching Next.js's route announcer; the corrected selector checks the specific business error and the suite passes.

Reproduce without Docker:

```sh
make e2e-native
```

The runner creates a disposable database, installs locked frontend dependencies, builds production assets, starts two workers, runs Chromium and cleans up its processes/database. It does not touch Neon or read `.env`.

Local evidence (generated artifacts are intentionally ignored by Git):

- [Complete native run](artifacts/native-e2e-full-run.log)
- [Go race/process test output](artifacts/native-e2e/go-tests.log)
- [Browser test output](artifacts/native-e2e/browser-tests.log)
- [Interactive Playwright report](artifacts/playwright/index.html)
- [Browser screenshots and videos](artifacts/test-results/)
- [Native launcher browser Retry screenshot](artifacts/dev-native-retry.png)
- [Fresh frontend installation and build](artifacts/clean-web-review.log)
- [Native E2E early-failure cleanup check](artifacts/native-cleanup-review.log)

The generated artifacts are available in the current workspace, not in the public repository. A fresh run generates them again; the GitHub Actions workflow uploads its own artifacts when executed. No hosted CI pass is claimed here.
