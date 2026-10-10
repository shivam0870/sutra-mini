"use client";

import { useCallback, useEffect, useRef, useState, type FormEvent } from "react";

type Status = "pending" | "pushed" | "failed";
type Issue = {
  id: number;
  party_name: string;
  sku_code: string;
  sku_name: string;
  issued: number;
  received: number;
  outstanding: number;
};
type Voucher = {
  id: number;
  source_id: number;
  amount_paise: number;
  status: Status;
  attempts: number;
  last_error: string | null;
  client_ref: string;
  external_ref: string | null;
  created_at: string;
  updated_at: string;
};
type ReceiptResult = { receipt: { id: number }; voucher: Voucher };

const groups: { status: Status; title: string; description: string; empty: string }[] = [
  { status: "pending", title: "Pending", description: "Queued for accounting", empty: "The queue is clear." },
  { status: "pushed", title: "Pushed", description: "Confirmed by accounting", empty: "Confirmed vouchers will appear here." },
  { status: "failed", title: "Failed", description: "Ready for your attention", empty: "No vouchers need attention." },
];
const money = new Intl.NumberFormat("en-IN", { style: "currency", currency: "INR" });

function today() {
  const date = new Date();
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(date.getDate()).padStart(2, "0")}`;
}

function requestKey() {
  // getRandomValues also works on local Docker HTTP hostnames, where randomUUID
  // is unavailable because the origin is not considered a secure context.
  return Array.from(crypto.getRandomValues(new Uint8Array(24)), (byte) => byte.toString(16).padStart(2, "0")).join("");
}

async function api<T>(path: string, options: RequestInit = {}): Promise<T> {
  const response = await fetch(`/api${path}`, { cache: "no-store", ...options });
  const result = await response.json().catch(() => null);
  if (!response.ok) throw new Error(result?.error || `Request failed (${response.status}). Please try again.`);
  if (result === null) throw new Error("The server returned an empty response. Please try again.");
  return result as T;
}

export default function SyncPage() {
  const [issues, setIssues] = useState<Issue[]>([]);
  const [vouchers, setVouchers] = useState<Voucher[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [loadError, setLoadError] = useState("");
  const [actionError, setActionError] = useState("");
  const [notice, setNotice] = useState("");
  const [issueID, setIssueID] = useState("");
  const [quantity, setQuantity] = useState("1");
  const [receivedOn, setReceivedOn] = useState("");
  const [saving, setSaving] = useState(false);
  const [retrying, setRetrying] = useState<number | null>(null);
  const [lastUpdated, setLastUpdated] = useState("");
  const inFlight = useRef(false);
  const busyAction = useRef(false);
  const mounted = useRef(false);
  const pollController = useRef<AbortController | null>(null);
  const actionController = useRef<AbortController | null>(null);
  // Keep the key for an uncertain request; changing the payload starts a new operation.
  const receiptRequest = useRef<{ payload: string; key: string } | null>(null);

  const refresh = useCallback(async () => {
    if (inFlight.current) return;
    inFlight.current = true;
    const controller = new AbortController();
    pollController.current = controller;
    let timedOut = false;
    const timeout = setTimeout(() => { timedOut = true; controller.abort(); }, 10000);
    try {
      const [issueResult, voucherResult] = await Promise.all([
        api<{ issues: Issue[] }>("/issues", { signal: controller.signal }),
        api<{ vouchers: Voucher[] }>("/vouchers", { signal: controller.signal }),
      ]);
      if (!mounted.current) return;
      setIssues(issueResult.issues || []);
      setVouchers(voucherResult.vouchers || []);
      setLoaded(true);
      setLoadError("");
      setLastUpdated(new Date().toLocaleTimeString("en-IN", { hour: "2-digit", minute: "2-digit", second: "2-digit" }));
      setIssueID((current) => current || String(issueResult.issues?.find((issue) => issue.outstanding > 0)?.id || ""));
    } catch (error) {
      if (mounted.current && (!controller.signal.aborted || timedOut)) setLoadError(timedOut ? "Refresh timed out. Reconnecting automatically…" : (error as Error).message);
    } finally {
      clearTimeout(timeout);
      // If one request failed, cancel its sibling before allowing the next poll.
      controller.abort();
      inFlight.current = false;
    }
  }, []);

  useEffect(() => {
    mounted.current = true;
    setReceivedOn(today());
    void refresh();
    const timer = setInterval(() => void refresh(), 2000);
    return () => {
      mounted.current = false;
      clearInterval(timer);
      pollController.current?.abort();
      actionController.current?.abort();
    };
  }, [refresh]);

  async function saveReceipt(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (busyAction.current) return;
    busyAction.current = true;
    setSaving(true);
    setActionError("");
    setNotice("");
    const payload = JSON.stringify({ issue_id: Number(issueID), qty: Number(quantity), received_on: receivedOn });
    if (!receiptRequest.current || receiptRequest.current.payload !== payload) {
      receiptRequest.current = { payload, key: requestKey() };
    }
    const controller = new AbortController();
    actionController.current = controller;
    const timeout = setTimeout(() => controller.abort(), 15000);
    try {
      const result = await api<ReceiptResult>("/jobwork/receipts", {
        method: "POST",
        signal: controller.signal,
        headers: { "Content-Type": "application/json", "Idempotency-Key": receiptRequest.current.key },
        body: payload,
      });
      if (!mounted.current) return;
      setNotice(`Receipt #${result.receipt.id} saved. Voucher #${result.voucher.id} is queued for accounting.`);
      receiptRequest.current = null;
      setQuantity("");
      await refresh();
    } catch (error) {
      if (mounted.current) setActionError(controller.signal.aborted ? "The request timed out. Submit again with the same details to safely check or finish this receipt." : (error as Error).message);
    } finally {
      clearTimeout(timeout);
      busyAction.current = false;
      if (mounted.current) setSaving(false);
    }
  }

  async function retryVoucher(id: number) {
    if (busyAction.current) return;
    busyAction.current = true;
    setRetrying(id);
    setActionError("");
    setNotice("");
    const controller = new AbortController();
    actionController.current = controller;
    const timeout = setTimeout(() => controller.abort(), 15000);
    try {
      const result = await api<{ voucher: Voucher }>(`/vouchers/${id}/retry`, { method: "POST", signal: controller.signal });
      if (!mounted.current) return;
      setVouchers((current) => current.map((voucher) => voucher.id === id ? result.voucher : voucher));
      setNotice(`Voucher #${id} queued again. Its status will update automatically.`);
      await refresh();
    } catch (error) {
      if (mounted.current) setActionError(controller.signal.aborted ? "Retry request timed out. Check the voucher status before trying again." : (error as Error).message);
    } finally {
      clearTimeout(timeout);
      busyAction.current = false;
      if (mounted.current) setRetrying(null);
    }
  }

  const selectedIssue = issues.find((issue) => String(issue.id) === issueID);
  const isBusy = saving || retrying !== null;
  const outstanding = issues.reduce((total, issue) => total + issue.outstanding, 0);

  return (
    <main className="shell">
      <header className="topbar">
        <a className="brand" href="/sync" aria-label="Sutra Mini home"><span className="brand-mark" aria-hidden="true">s</span><span>Sutra <span className="brand-sub">mini</span></span></a>
        <span className="studio">SHREE RADHA STUDIO</span>
      </header>

      <div className="page-intro">
        <div><p className="eyebrow">JOB-WORK OPERATIONS</p><h1>Accounting sync</h1><p className="intro-copy">Record what comes back. Keep every receipt accounted for.</p></div>
        <div className={`live-status ${loadError ? "disconnected" : ""}`}><span className="live-dot" aria-hidden="true" /><span>{loadError ? "Reconnecting" : loaded ? "Live · updates every 2s" : "Connecting…"}{lastUpdated && <small>Last checked {lastUpdated}</small>}</span></div>
      </div>

      {loadError && <div className="alert error" role="alert">{loadError} {loaded && "Showing the last available data."}</div>}
      {actionError && <div className="alert error" role="alert">{actionError}</div>}
      <div aria-live="polite" aria-atomic="true">{notice && <div className="alert success" role="status">{notice}</div>}</div>

      <section className="summary" aria-label="Sync summary">
        <div className="metric"><span>Pieces outstanding</span><strong>{loaded ? outstanding.toLocaleString("en-IN") : "—"}</strong><small>Across {issues.length} issues</small></div>
        {groups.map((group) => <div className={`metric ${group.status}`} key={group.status}><span><i aria-hidden="true" />{group.title}</span><strong>{loaded ? vouchers.filter((voucher) => voucher.status === group.status).length : "—"}</strong><small>{group.description}</small></div>)}
      </section>

      <section className="receipt-panel" aria-labelledby="receipt-title">
        <div className="section-heading"><div><p className="eyebrow">RECEIVE JOB-WORK</p><h2 id="receipt-title">New receipt</h2><p>A receipt and its accounting voucher are saved together.</p></div></div>
        <form onSubmit={saveReceipt}>
          <label className="issue-field">Open issue<select data-testid="issue-select" value={issueID} onChange={(event) => setIssueID(event.target.value)} required disabled={!loaded || isBusy}>
            <option value="" disabled>Select an issue</option>
            {issues.map((issue) => <option key={issue.id} value={issue.id} disabled={issue.outstanding <= 0}>#{issue.id} · {issue.sku_code} · {issue.party_name} ({issue.outstanding} outstanding)</option>)}
          </select></label>
          <label>Quantity<input data-testid="qty" type="number" value={quantity} onChange={(event) => setQuantity(event.target.value)} min="1" step="1" required disabled={isBusy} placeholder="Pieces" /></label>
          <label>Received on<input data-testid="received-on" type="date" value={receivedOn} onChange={(event) => setReceivedOn(event.target.value)} required disabled={isBusy} /></label>
          <button className="primary" type="submit" disabled={!loaded || !selectedIssue || (selectedIssue.outstanding <= 0 && !receiptRequest.current) || isBusy}>{saving ? "Saving…" : "Save receipt"}<span aria-hidden="true">↗</span></button>
        </form>
        {selectedIssue && <p className="form-hint">{selectedIssue.sku_name} · {selectedIssue.received} of {selectedIssue.issued} received · <strong>{selectedIssue.outstanding} still with {selectedIssue.party_name}</strong></p>}
      </section>

      <div className="board-heading"><h2>Voucher activity</h2><span>{vouchers.length} total vouchers</span></div>
      <div className="voucher-board" aria-busy={!loaded}>
        {groups.map((group) => {
          const items = vouchers.filter((voucher) => voucher.status === group.status);
          return <section className={`voucher-column ${group.status}`} key={group.status} aria-labelledby={`${group.status}-title`} data-testid={`${group.status}-group`}>
            <div className="column-heading"><h3 id={`${group.status}-title`}><i aria-hidden="true" />{group.title}<span className="count">{items.length}</span></h3><p>{group.description}</p></div>
            <div className="voucher-list">
              {!loaded && <p className="empty-state">Loading vouchers…</p>}
              {loaded && items.length === 0 && <p className="empty-state">{group.empty}</p>}
              {items.map((voucher) => <article key={voucher.id} className="voucher-card" data-testid={`voucher-${voucher.id}`} data-status={voucher.status}>
                <div className="voucher-title"><h4>Voucher #{voucher.id}</h4><strong>{money.format(voucher.amount_paise / 100)}</strong></div>
                <p className="receipt-reference">Receipt #{voucher.source_id}</p>
                <dl><div><dt>Attempts</dt><dd>{voucher.attempts}</dd></div><div><dt>Client reference</dt><dd className="mono">{voucher.client_ref}</dd></div>{voucher.external_ref && <div><dt>Accounting reference</dt><dd className="mono">{voucher.external_ref}</dd></div>}</dl>
                {voucher.last_error && <p className="failure-reason"><span>Last error</span>{voucher.last_error}</p>}
                <div className="voucher-footer"><time dateTime={voucher.created_at}>{new Date(voucher.created_at).toLocaleString("en-IN", { day: "2-digit", month: "short", hour: "2-digit", minute: "2-digit" })}</time>{voucher.status === "failed" && <button type="button" className="retry" onClick={() => void retryVoucher(voucher.id)} disabled={isBusy} aria-label={`Retry voucher ${voucher.id}`}>{retrying === voucher.id ? "Retrying…" : "Retry"}<span aria-hidden="true">↻</span></button>}{voucher.status === "pushed" && <span className="confirmed">✓ Confirmed</span>}{voucher.status === "pending" && <span className="queued">In queue</span>}</div>
              </article>)}
            </div>
          </section>;
        })}
      </div>
      <footer className="page-footer"><span>Sutra Mini</span><span>Every receipt, accounted for.</span></footer>
    </main>
  );
}
