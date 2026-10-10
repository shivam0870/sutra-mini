import { expect, test } from "@playwright/test";
import { randomUUID } from "node:crypto";

type Issue = { id: number; outstanding: number; received: number; issued: number };
type Voucher = { id: number; source_id: number; status: string; attempts: number; last_error: string | null; client_ref: string; external_ref: string | null };

// Run against the real Go API, Postgres, mock accounting and worker processes.
// The test stack seeds one failed voucher; no API or browser responses are stubbed.
test.describe.configure({ mode: "serial" });

test.afterEach(async ({ page }, testInfo) => {
  if (page.url().endsWith("/sync")) {
    await page.screenshot({ path: testInfo.outputPath("sync.png"), fullPage: true });
  }
});

test("receipt form saves a receipt and its voucher reaches accounting", async ({ page, request }) => {
  const issueResponse = await request.get("/api/issues");
  expect(issueResponse.ok()).toBeTruthy();
  const { issues } = await issueResponse.json() as { issues: Issue[] };
  const issue = issues.find((item) => item.outstanding >= 3);
  expect(issue, "Seed at least one issue with three outstanding pieces").toBeTruthy();

  await page.goto("/sync");
  await expect(page.getByRole("heading", { name: "Accounting sync" })).toBeVisible();
  for (const status of ["pending", "pushed", "failed"]) {
    await expect(page.getByTestId(`${status}-group`)).toBeVisible();
  }
  await page.getByTestId("issue-select").selectOption(String(issue!.id));
  await page.getByTestId("qty").fill("1");
  await page.getByTestId("received-on").fill("2026-10-10");
  const saved = page.waitForResponse((response) => response.url().endsWith("/api/jobwork/receipts") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Save receipt" }).click();
  const response = await saved;
  expect(response.status()).toBe(201);
  const result = await response.json();
  await expect(page.getByRole("status")).toContainText(`Receipt #${result.receipt.id} saved`);
  await expect(page.getByTestId(`voucher-${result.voucher.id}`)).toHaveAttribute("data-status", "pushed");
  await expect(page.getByTestId(`voucher-${result.voucher.id}`)).toContainText("Accounting reference");
  const refreshed = await (await request.get(`/api/issues/${issue!.id}`)).json() as Issue;
  expect(refreshed.received).toBe(issue!.received + 1);
});

test("replaying a key keeps one receipt and one voucher; changed payload conflicts", async ({ request }) => {
  const { issues } = await (await request.get("/api/issues")).json() as { issues: Issue[] };
  const issue = issues.find((item) => item.outstanding >= 2)!;
  expect(issue).toBeTruthy();
  const key = `browser-e2e-${randomUUID()}`;
  const data = { issue_id: issue.id, qty: 1, received_on: "2026-10-10" };
  const headers = { "Idempotency-Key": key };
  const first = await request.post("/api/jobwork/receipts", { data, headers });
  expect(first.status()).toBe(201);
  const original = await first.json();
  const replay = await request.post("/api/jobwork/receipts", { data, headers });
  expect(replay.status()).toBe(201);
  expect(replay.headers()["idempotency-replayed"]).toBe("true");
  const replayed = await replay.json();
  expect(replayed.receipt.id).toBe(original.receipt.id);
  expect(replayed.voucher.id).toBe(original.voucher.id);
  const changed = await request.post("/api/jobwork/receipts", { data: { ...data, qty: 2 }, headers });
  expect(changed.status()).toBe(409);
  const refreshed = await (await request.get(`/api/issues/${issue.id}`)).json() as Issue;
  expect(refreshed.received).toBe(issue.received + 1);
  const { vouchers } = await (await request.get("/api/vouchers")).json() as { vouchers: Voucher[] };
  expect(vouchers.filter((voucher) => voucher.source_id === original.receipt.id)).toHaveLength(1);
});

test("the form displays the API overshoot rejection without creating a voucher", async ({ page, request }) => {
  const { issues } = await (await request.get("/api/issues")).json() as { issues: Issue[] };
  const issue = issues.find((item) => item.outstanding > 0)!;
  expect(issue).toBeTruthy();
  const before = await (await request.get("/api/vouchers")).json() as { vouchers: Voucher[] };
  await page.goto("/sync");
  await page.getByTestId("issue-select").selectOption(String(issue.id));
  await page.getByTestId("qty").fill(String(issue.outstanding + 1));
  const rejected = page.waitForResponse((response) => response.url().endsWith("/api/jobwork/receipts") && response.request().method() === "POST");
  await page.getByRole("button", { name: "Save receipt" }).click();
  expect((await rejected).status()).toBe(409);
  await expect(page.getByRole("alert").filter({ hasText: "receipt exceeds the issue's outstanding quantity" })).toBeVisible();
  const after = await (await request.get("/api/vouchers")).json() as { vouchers: Voucher[] };
  expect(after.vouchers.map((voucher) => voucher.id).sort()).toEqual(before.vouchers.map((voucher) => voucher.id).sort());
  const refreshed = await (await request.get(`/api/issues/${issue.id}`)).json() as Issue;
  expect(refreshed.received).toBe(issue.received);
});

test("Retry moves a failed voucher to pushed with an accounting reference", async ({ page, request }) => {
  const { vouchers } = await (await request.get("/api/vouchers?status=failed")).json() as { vouchers: Voucher[] };
  const voucher = vouchers.find((item) => item.last_error === "E2E accounting outage");
  expect(voucher, "Run scripts/e2e.sh to prepare the failed voucher fixture").toBeTruthy();
  await page.goto("/sync");
  const card = page.getByTestId(`voucher-${voucher!.id}`);
  await expect(card).toHaveAttribute("data-status", "failed");
  await expect(card).toContainText("E2E accounting outage");
  const retry = page.waitForResponse((response) => response.url().endsWith(`/api/vouchers/${voucher!.id}/retry`) && response.request().method() === "POST");
  await page.getByRole("button", { name: `Retry voucher ${voucher!.id}`, exact: true }).click();
  expect((await retry).ok()).toBeTruthy();
  await expect(card).toHaveAttribute("data-status", "pushed");
  await expect(card).toContainText("Accounting reference");
  const { vouchers: updated } = await (await request.get("/api/vouchers")).json() as { vouchers: Voucher[] };
  const pushed = updated.find((item) => item.id === voucher!.id)!;
  expect(pushed.status).toBe("pushed");
  expect(pushed.client_ref).toBe(voucher!.client_ref);
  expect(pushed.external_ref).toBeTruthy();
});
