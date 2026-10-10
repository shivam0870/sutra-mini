package mockaccounting

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sutra/internal/testdb"
)

func testServer(t *testing.T, pool *pgxpool.Pool, mode string, hang time.Duration) *httptest.Server {
	t.Helper()
	mock, err := New(pool, Config{Mode: mode, HangDuration: hang}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(mock.Handler())
	t.Cleanup(server.Close)
	return server
}

func postVoucher(client *http.Client, endpoint, ref string) (int, string, error) {
	body := fmt.Sprintf(`{"client_ref":%q,"type":"jobwork_receipt","amount_paise":12500}`, ref)
	resp, err := client.Post(endpoint+"/api/vouchers", "application/json", strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	var result struct {
		ExternalRef string `json:"external_ref"`
	}
	err = json.NewDecoder(resp.Body).Decode(&result)
	return resp.StatusCode, result.ExternalRef, err
}

func TestConcurrentDuplicatesPersistExactlyOneAccountingEntry(t *testing.T) {
	pool := testdb.New(t)
	server := testServer(t, pool, "success", time.Millisecond)
	type result struct {
		status int
		ref    string
		err    error
	}
	results := make(chan result, 20)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, ref, err := postVoucher(server.Client(), server.URL, "one-posting")
			results <- result{status, ref, err}
		}()
	}
	wg.Wait()
	close(results)
	created, duplicates := 0, 0
	var originalRef string
	for result := range results {
		if result.err != nil || result.ref == "" {
			t.Fatalf("bad response: %+v", result)
		}
		if originalRef == "" {
			originalRef = result.ref
		}
		if result.ref != originalRef {
			t.Fatalf("duplicate reference changed: %q != %q", result.ref, originalRef)
		}
		switch result.status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			duplicates++
		default:
			t.Fatalf("unexpected HTTP status %d", result.status)
		}
	}
	if created != 1 || duplicates != 19 {
		t.Fatalf("created=%d duplicates=%d, want 1 and 19", created, duplicates)
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM mock_accounting_vouchers`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("ledger must contain exactly one row: count=%d err=%v", count, err)
	}
	// A fresh server instance in failure mode must still remember accepted refs.
	restarted := testServer(t, pool, "fail", time.Millisecond)
	status, ref, err := postVoucher(restarted.Client(), restarted.URL, "one-posting")
	if err != nil || status != http.StatusConflict || ref != originalRef {
		t.Fatalf("restart lost persistent idempotency: status=%d ref=%q err=%v", status, ref, err)
	}
}

func TestHangPersistsBeforeResponseAndReconcilesImmediately(t *testing.T) {
	pool := testdb.New(t)
	server := testServer(t, pool, "hang", 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/vouchers", strings.NewReader(`{"client_ref":"uncertain-outcome","type":"jobwork_receipt","amount_paise":12500}`))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		response, err := server.Client().Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	var storedRef string
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		err := pool.QueryRow(context.Background(), `SELECT external_ref FROM mock_accounting_vouchers WHERE client_ref='uncertain-outcome'`).Scan(&storedRef)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if storedRef == "" {
		t.Fatal("accounting did not persist before hanging")
	}
	select {
	case err := <-done:
		t.Fatalf("response returned before its five-second delay: %v", err)
	default:
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled request unexpectedly completed")
	}
	client := &http.Client{Timeout: time.Second}
	status, ref, err := postVoucher(client, server.URL, "uncertain-outcome")
	if err != nil || status != http.StatusConflict || ref != storedRef {
		t.Fatalf("duplicate should return immediately with original reference: status=%d ref=%q err=%v", status, ref, err)
	}
}

func TestFailureDoesNotCreateAccountingEntry(t *testing.T) {
	pool := testdb.New(t)
	server := testServer(t, pool, "fail", time.Millisecond)
	status, _, err := postVoucher(server.Client(), server.URL, "failed-push")
	if err != nil || status != http.StatusInternalServerError {
		t.Fatalf("failure mode: status=%d err=%v", status, err)
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM mock_accounting_vouchers`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed call created accounting state: count=%d err=%v", count, err)
	}
}
