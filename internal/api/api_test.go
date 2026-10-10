package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"sutra/internal/api"
	"sutra/internal/testdb"
)

func postReceipt(handler http.Handler, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/jobwork/receipts", bytes.NewBufferString(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// Ten simultaneous requests could receive 150 units against an issue of 100.
// Assert exactly six commits and four conflicts, plus matching ledger/counter
// and voucher counts; an error-only implementation cannot make this test pass.
func TestConcurrentReceiptsCannotOvershoot(t *testing.T) {
	pool := testdb.New(t)
	handler := api.New(pool)
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 10)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			responses <- postReceipt(handler, fmt.Sprintf("concurrent-%d", index), `{"issue_id":1,"qty":15,"received_on":"2026-10-10"}`)
		}(i)
	}
	close(start)
	wg.Wait()
	close(responses)
	success, conflict := 0, 0
	for response := range responses {
		switch response.Code {
		case http.StatusCreated:
			success++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("unexpected HTTP %d: %s", response.Code, response.Body.String())
		}
	}
	if success != 6 || conflict != 4 {
		t.Fatalf("success=%d conflict=%d, want 6 and 4", success, conflict)
	}
	assertCounts(t, pool, 6, 6, 90)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/issues/1", nil))
	var issue api.Issue
	if err := json.Unmarshal(response.Body.Bytes(), &issue); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || issue.Issued != 100 || issue.Received != 90 || issue.Outstanding != 10 {
		t.Fatalf("incorrect issue totals: HTTP %d %s", response.Code, response.Body.String())
	}
}

func TestIdempotencyReplayIsExactAndCreatesNothing(t *testing.T) {
	pool := testdb.New(t)
	handler := api.New(pool)
	body := `{"issue_id":1,"qty":100,"received_on":"2026-10-10"}`
	first := postReceipt(handler, "replay-key", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first response: HTTP %d %s", first.Code, first.Body.String())
	}
	// Simulate asynchronous worker progress. Replay must return the original
	// saved response, even though the current voucher is no longer pending.
	if _, err := pool.Exec(context.Background(), `UPDATE vouchers SET status='pushed',external_ref='EXT-test',attempts=1`); err != nil {
		t.Fatal(err)
	}
	second := postReceipt(handler, "replay-key", body)
	if second.Code != first.Code || second.Body.String() != first.Body.String() || second.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay changed result: first=%d %s second=%d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	assertCounts(t, pool, 1, 1, 100)
	changed := postReceipt(handler, "replay-key", `{"issue_id":1,"qty":99,"received_on":"2026-10-10"}`)
	if changed.Code != http.StatusConflict {
		t.Fatalf("changed payload should conflict, got %d %s", changed.Code, changed.Body.String())
	}
	assertCounts(t, pool, 1, 1, 100)
}

func TestConcurrentSameKeyCreatesOneReceiptAndVoucher(t *testing.T) {
	pool := testdb.New(t)
	handler := api.New(pool)
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 10)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses <- postReceipt(handler, "same-key", `{"issue_id":1,"qty":100,"received_on":"2026-10-10"}`)
		}()
	}
	close(start)
	wg.Wait()
	close(responses)
	var first string
	for response := range responses {
		if response.Code != http.StatusCreated {
			t.Fatalf("expected successful replay, got %d %s", response.Code, response.Body.String())
		}
		if first == "" {
			first = response.Body.String()
		} else if response.Body.String() != first {
			t.Fatal("concurrent replay returned different response")
		}
	}
	assertCounts(t, pool, 1, 1, 100)
}

func TestVoucherFailureRollsBackReceiptAndCapacity(t *testing.T) {
	pool := testdb.New(t)
	handler := api.New(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `ALTER TABLE vouchers ADD CONSTRAINT fail_test_insert CHECK (amount_paise < 1)`); err != nil {
		t.Fatal(err)
	}
	response := postReceipt(handler, "rollback-key", `{"issue_id":1,"qty":20,"received_on":"2026-10-10"}`)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("expected voucher constraint failure, got %d %s", response.Code, response.Body.String())
	}
	assertCounts(t, pool, 0, 0, 0)
	if _, err := pool.Exec(ctx, `ALTER TABLE vouchers DROP CONSTRAINT fail_test_insert`); err != nil {
		t.Fatal(err)
	}
	response = postReceipt(handler, "rollback-key", `{"issue_id":1,"qty":20,"received_on":"2026-10-10"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("rolled back key must be reusable: %d %s", response.Code, response.Body.String())
	}
	assertCounts(t, pool, 1, 1, 20)
}

func TestDatabaseTriggerRejectsDirectOvershootAndLedgerEdits(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	insert := `INSERT INTO jobwork_receipts(issue_id,qty,received_on,idempotency_key,request_fingerprint,response_json)
        VALUES (1,$1,CURRENT_DATE,$2,'test','{}'::jsonb)`
	if _, err := pool.Exec(ctx, insert, 100, "direct-full"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, insert, 1, "direct-overshoot"); err == nil {
		t.Fatal("direct database insert bypassed capacity invariant")
	}
	_, err := pool.Exec(ctx, `INSERT INTO jobwork_receipts(issue_id,qty,received_on,idempotency_key,request_fingerprint,response_json)
        VALUES (2,1,CURRENT_DATE,'direct-full','test','{}'::jsonb)`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("database must enforce key uniqueness across issues, got %v", err)
	}
	var otherReceived int64
	if err := pool.QueryRow(ctx, `SELECT received_qty FROM jobwork_issues WHERE id=2`).Scan(&otherReceived); err != nil {
		t.Fatal(err)
	}
	if otherReceived != 0 {
		t.Fatal("unique conflict leaked reserved capacity onto another issue")
	}
	if _, err := pool.Exec(ctx, `UPDATE jobwork_receipts SET qty=1`); err == nil {
		t.Fatal("database allowed receipt ledger mutation")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM jobwork_receipts`); err == nil {
		t.Fatal("database allowed receipt ledger deletion")
	}
	if _, err := pool.Exec(ctx, `UPDATE jobwork_issues SET qty=99 WHERE id=1`); err == nil {
		t.Fatal("database allowed issue quantity below received total")
	}
	assertCounts(t, pool, 1, 0, 100)
}

func TestRetryPreservesIdentityAndRejectsNonFailed(t *testing.T) {
	pool := testdb.New(t)
	handler := api.New(pool)
	created := postReceipt(handler, "retry-test", `{"issue_id":1,"qty":1,"received_on":"2026-10-10"}`)
	if created.Code != http.StatusCreated {
		t.Fatal(created.Body.String())
	}
	var response struct {
		Voucher api.Voucher `json:"voucher"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("/vouchers/%d/retry", response.Voucher.ID)
	postRetry := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, url, nil))
		return w
	}
	if got := postRetry(); got.Code != http.StatusConflict {
		t.Fatalf("pending retry should conflict: %d", got.Code)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE vouchers SET status='failed',attempts=5,last_error='HTTP 500' WHERE id=$1`, response.Voucher.ID); err != nil {
		t.Fatal(err)
	}
	got := postRetry()
	var retried struct {
		Voucher api.Voucher `json:"voucher"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &retried); err != nil {
		t.Fatal(err)
	}
	if got.Code != http.StatusOK || retried.Voucher.Status != "pending" || retried.Voucher.Attempts != 0 || retried.Voucher.LastError != nil || retried.Voucher.ClientRef != response.Voucher.ClientRef {
		t.Fatalf("bad retry response: %d %s", got.Code, got.Body.String())
	}
}

func TestRequestValidation(t *testing.T) {
	// Validation must happen before opening a database transaction.
	handler := api.New(nil)
	for _, tc := range []struct{ key, body string }{
		{"", `{"issue_id":1,"qty":1,"received_on":"2026-10-10"}`},
		{"key", `{"issue_id":1,"qty":0,"received_on":"2026-10-10"}`},
		{"key", `{"issue_id":1,"qty":1.5,"received_on":"2026-10-10"}`},
		{"key", `{"issue_id":1,"qty":9223372036854775807,"received_on":"2026-10-10"}`},
		{"key", `{"issue_id":1,"qty":1,"received_on":"not-a-date"}`},
		{"key", `{"issue_id":1,"qty":1,"received_on":"2026-10-10","unknown":true}`},
		{"key", `{"issue_id":1,"qty":1,"received_on":"2026-10-10"}{}`},
		{"key", `null`},
	} {
		if got := postReceipt(handler, tc.key, tc.body); got.Code != http.StatusBadRequest {
			t.Fatalf("body %q: wanted400 got%d", tc.body, got.Code)
		}
	}
}

func assertCounts(t *testing.T, pool *pgxpool.Pool, receipts, vouchers, received int64) {
	t.Helper()
	var actualReceipts, actualVouchers, actualReceived, ledgerTotal int64
	err := pool.QueryRow(context.Background(), `SELECT
        (SELECT count(*) FROM jobwork_receipts), (SELECT count(*) FROM vouchers),
        (SELECT received_qty FROM jobwork_issues WHERE id=1),
        (SELECT COALESCE(sum(qty),0) FROM jobwork_receipts WHERE issue_id=1)`).Scan(
		&actualReceipts, &actualVouchers, &actualReceived, &ledgerTotal)
	if err != nil {
		t.Fatal(err)
	}
	if actualReceipts != receipts || actualVouchers != vouchers || actualReceived != received || ledgerTotal != received {
		t.Fatalf("got receipts=%d vouchers=%d counter=%d ledger=%d; want %d,%d,%d,%d",
			actualReceipts, actualVouchers, actualReceived, ledgerTotal, receipts, vouchers, received, received)
	}
}
