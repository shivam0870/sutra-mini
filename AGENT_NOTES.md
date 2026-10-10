# Agent notes

## Tools and delegation

Built with Codex and three parallel sub-agents at the user's explicit request. One owned the Go API, migrations and transactional tests; one owned worker reliability, the mock and their tests; one owned the Next.js page and browser tests. The coordinating agent defined shared contracts, reviewed the implementation, added actual-process crash E2E tests, Docker/CI orchestration, and these instructions.

The session was interrupted by a server restart. Persisted files were inspected and the three unfinished tasks were reassigned; work was not assumed to have survived in agent memory.

Tools used: local shell, source editing, primary-source web lookups, Go 1.26.9, PostgreSQL 17.11, npm, Next.js production builds, Playwright Chromium, Docker Compose configuration validation, and initially offline syntax parsers. The Go archive was verified against the official release SHA-256. No LLM is used by the application itself. The optional `/ask` feature was deferred.

## What the agents got wrong and what review caught

- The initial UI generated keys with `crypto.randomUUID()`. That method requires a secure browser context, so a browser test at `http://web:3000` could fail before submitting. Review caught it; the page now builds a random key from `crypto.getRandomValues`, including on ordinary HTTP Docker hostnames. Uncertain retries retain their key while the payload is unchanged.
- The first API insert passed a string to a PostgreSQL parameter inferred as `date`. Review changed it to the parsed Go `time.Time` so pgx's binary date codec receives the correct type.
- The first SQL worker lease calculation left interval parameters implicitly typed; explicit bigint casts now match the millisecond integers passed by pgx.
- Initial worker error truncation could split a UTF-8 character or preserve a NUL, making PostgreSQL reject the error update. It now normalizes UTF-8, removes NULs and truncates on a character boundary, with regression coverage.
- The original environment plan assumed Go installation and local PostgreSQL could be started. The first session's sandbox blocked downloads/shared memory. After the user enabled full access, fresh agents installed dependencies and ran the real suite. Existing agents retained the earlier permissions and had to be replaced. A native test runner was added so Docker is optional.
- The first real browser run failed because `getByRole("alert")` matched both the application error and Next.js's hidden route announcer. The selector now checks the actual overshoot error text. The application correctly rejected the excess receipt throughout; after the selector fix all four browser tests passed, including Retry.
- A final portability check found that macOS Bash 3.2 treats the E2E runner's empty process array as unbound under `set -u`. An early startup failure could therefore interrupt cleanup. The runner now safely expands an empty array; a real PostgreSQL startup failure was exercised to verify the original exit status and removal of the temporary database directory. A fresh frontend copy also passed dependency installation, typecheck and production build without cached workspace assets.

## Decisions worth understanding before the interview

The receipt counter trigger reserves capacity using a conditional update of the parent issue. A competing insert waits for the row lock, then sees updated capacity. Transaction rollback also rolls back the counter, so a voucher insert error cannot strand received quantity.

Idempotency is a database protocol: transaction advisory lock, unique key, fingerprint, and saved response. A replay returns the original snapshot; the status listing returns current status. These serve different purposes.

An outbox cannot guarantee exactly one HTTP request across a crash. The worker deliberately repeats an uncertain request with the same client reference. The external service's unique key and original-reference `409` response make the accounting effect happen once. A new random reference on retry would break that guarantee.

Leases alone are insufficient: old work might finish after another worker has taken over. A fresh random token on each claim fences every database completion update. Each free slot claims a single row; there is no reserved batch waiting for a slot while its leases expire.

The mock writes to its own ledger before the five-second delayed response. This models an accepted accounting transaction whose caller never receives confirmation. It shares the database host for a small demo, but the worker accesses it over HTTP and never reads the mock ledger for its delivery logic.

## Actual verification and remaining work

The final submission review used separate agents for a read-only code audit and a native development launcher. The launcher was exercised with a real failing mock, a restart preserving the failed voucher, and a successful browser Retry. An additional check verified receipt replay and accounting duplicate responses. It also supports an exported database URL; that path was tested against local PostgreSQL, not hosted Neon. Source exclusions and a production dependency audit were checked before preparing submission instructions.

Passed against real PostgreSQL 17 on 2026-10-10: `go mod tidy`, `gofmt`, `go vet ./...`, Go compilation, and `go test -race -count=1 -v ./...`: **23 top-level tests and 8 subtests**, no skips or races. This includes the actual worker SIGKILL during the five-second accounting response, two replacement processes, and exactly one accounting posting. Logs are in `artifacts/go-tests.log` and `artifacts/native-e2e/go-tests.log`.

Also passed: TypeScript typecheck, Next.js production build and **4 Chromium E2E tests** with the real Go API, real PostgreSQL, mock accounting server and two worker processes. Tests cover receipt → pushed, replay/conflicting reuse, overshoot rejection and failed → Retry → pushed. Browser HTML report, screenshots and real videos are saved under `artifacts/`. The page was visually inspected from the captured screenshot.

Go dependencies were downloaded and verified by the toolchain. `web/package-lock.json` was generated by a real successful npm installation; Docker now requires `npm ci`. Docker Compose configuration validates, but the additional Docker runtime attempt stopped at an upstream VM image download connection reset. The temporary Colima profile was removed and the user's Docker context was left unchanged. The native E2E runner is fully functional without that VM.

The code is published at [shivam0870/sutra-mini](https://github.com/shivam0870/sutra-mini). A hosted Neon project and narrated three-minute submission recording remain outstanding; actual automated browser recordings are available locally. No submission email was sent by this workflow. Credentials are not included. The fixed ₹25-per-piece accounting rate is an explicit assumption, not a business rule supplied by the brief.
