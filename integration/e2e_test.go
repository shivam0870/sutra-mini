// These tests use real PostgreSQL, the real API/mock handlers and real worker
// subprocesses. TEST_DATABASE_URL must point to a disposable PostgreSQL database.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"sutra/internal/api"
	"sutra/internal/mockaccounting"
	"sutra/migrations"
)

type fixture struct {
	pool   *pgxpool.Pool
	dsn    string
	api    *httptest.Server
	binary string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is required for process end-to-end tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("e2e_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("TEST_DATABASE_URL must be a postgres:// or postgresql:// URL")
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	f := &fixture{dsn: parsed.String()}
	f.pool, err = pgxpool.New(ctx, f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.pool.Close)
	if err := migrations.Apply(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	f.api = httptest.NewServer(api.New(f.pool))
	t.Cleanup(f.api.Close)
	f.binary = filepath.Join(t.TempDir(), "worker")
	build := exec.Command("go", "build", "-o", f.binary, "./cmd/worker")
	build.Dir = ".."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build worker: %v\n%s", err, output)
	}
	return f
}

func (f *fixture) mock(t *testing.T, mode string) http.Handler {
	t.Helper()
	server, err := mockaccounting.New(f.pool, mockaccounting.Config{Mode: mode, HangDuration: 5 * time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler()
}

func (f *fixture) startWorker(t *testing.T, endpoint string) *exec.Cmd {
	t.Helper()
	command := exec.Command(f.binary)
	overrides := map[string]string{
		"DATABASE_URL": f.dsn, "ACCOUNTING_URL": endpoint,
		"WORKER_CONCURRENCY": "4", "WORKER_HTTP_TIMEOUT": "1s",
		"WORKER_LEASE_DURATION": "2300ms", "WORKER_POLL_INTERVAL": "20ms",
		"WORKER_BASE_BACKOFF": "30ms", "WORKER_MAX_BACKOFF": "100ms",
		"WORKER_MAX_ATTEMPTS": "3",
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			command.Env = append(command.Env, entry)
		}
	}
	for key, value := range overrides {
		command.Env = append(command.Env, key+"="+value)
	}
	logFile, err := os.CreateTemp(t.TempDir(), "worker-*.log")
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout, command.Stderr = logFile, logFile
	if err := command.Start(); err != nil {
		logFile.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = logFile.Close()
		if t.Failed() {
			log, _ := os.ReadFile(logFile.Name())
			t.Logf("worker process log:\n%s", log)
		}
	})
	return command
}

func request(t *testing.T, method, endpoint, key string, body any, want int) []byte {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, endpoint, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s: got %d want %d: %s", method, endpoint, response.StatusCode, want, result)
	}
	return result
}

type created struct {
	Receipt struct {
		ID int64 `json:"id"`
	} `json:"receipt"`
	Voucher struct {
		ID        int64  `json:"id"`
		ClientRef string `json:"client_ref"`
	} `json:"voucher"`
}

func (f *fixture) receive(t *testing.T, key string) created {
	t.Helper()
	body := map[string]any{"issue_id": 1, "qty": 1, "received_on": time.Now().Format("2006-01-02")}
	raw := request(t, "POST", f.api.URL+"/jobwork/receipts", key, body, http.StatusCreated)
	var result created
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Voucher.ID == 0 || result.Voucher.ClientRef == "" {
		t.Fatalf("invalid receipt response: %s", raw)
	}
	return result
}

func eventually(t *testing.T, message string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out: " + message)
}

func (f *fixture) state(id int64) (status, reference string, attempts int) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = f.pool.QueryRow(ctx, `SELECT status, coalesce(external_ref,''), attempts FROM vouchers WHERE id=$1`, id).Scan(&status, &reference, &attempts)
	return
}

func TestWorkerProcessCrashDuringHangAndTwoRestarts(t *testing.T) {
	f := setup(t)
	mock := httptest.NewServer(f.mock(t, "hang"))
	t.Cleanup(mock.Close)
	result := f.receive(t, "crash-during-five-second-hang")
	first := f.startWorker(t, mock.URL+"/api/vouchers")
	var originalReference string
	eventually(t, "mock must durably accept before the delayed response", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return f.pool.QueryRow(ctx, `SELECT external_ref FROM mock_accounting_vouchers WHERE client_ref=$1`, result.Voucher.ClientRef).Scan(&originalReference) == nil
	})
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	status, _, _ := f.state(result.Voucher.ID)
	if status != "pending" {
		t.Fatalf("worker must die before saving success; got %q", status)
	}
	f.startWorker(t, mock.URL+"/api/vouchers")
	f.startWorker(t, mock.URL+"/api/vouchers")
	eventually(t, "a replacement worker must reconcile the 409 duplicate", func() bool {
		status, ref, attempts := f.state(result.Voucher.ID)
		return status == "pushed" && ref == originalReference && attempts >= 2
	})
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM mock_accounting_vouchers WHERE client_ref=$1`, result.Voucher.ClientRef).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("financial posting duplicated: count=%d", count)
	}
	// Check the actual duplicate endpoint response, not just the local status.
	raw := request(t, "POST", mock.URL+"/api/vouchers", "", map[string]any{"client_ref": result.Voucher.ClientRef, "type": "jobwork_receipt", "amount_paise": 2500}, http.StatusConflict)
	var duplicate struct {
		ExternalRef string `json:"external_ref"`
	}
	if err := json.Unmarshal(raw, &duplicate); err != nil || duplicate.ExternalRef != originalReference {
		t.Fatalf("409 must return original reference: %s (%v)", raw, err)
	}
}

func TestEndToEndFailedVoucherRetryAndTwoWorkers(t *testing.T) {
	f := setup(t)
	fail, success := f.mock(t, "fail"), f.mock(t, "success")
	var recovered atomic.Bool
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if recovered.Load() {
			success.ServeHTTP(w, r)
		} else {
			fail.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(mock.Close)
	f.startWorker(t, mock.URL+"/api/vouchers")
	f.startWorker(t, mock.URL+"/api/vouchers")
	result := f.receive(t, "failure-and-manual-retry")
	eventually(t, "three failures must become failed with a reason", func() bool {
		var reason string
		status, _, attempts := f.state(result.Voucher.ID)
		_ = f.pool.QueryRow(context.Background(), `SELECT coalesce(last_error,'') FROM vouchers WHERE id=$1`, result.Voucher.ID).Scan(&reason)
		return status == "failed" && attempts == 3 && strings.Contains(reason, "500")
	})
	recovered.Store(true)
	request(t, "POST", fmt.Sprintf("%s/vouchers/%d/retry", f.api.URL, result.Voucher.ID), "", nil, http.StatusOK)
	eventually(t, "retried voucher must be pushed", func() bool {
		status, ref, attempts := f.state(result.Voucher.ID)
		return status == "pushed" && ref != "" && attempts == 1
	})
	var refs []string
	refs = append(refs, result.Voucher.ClientRef)
	for i := 0; i < 20; i++ {
		refs = append(refs, f.receive(t, fmt.Sprintf("two-workers-%d", i)).Voucher.ClientRef)
	}
	eventually(t, "both workers must drain all 21 vouchers", func() bool {
		var count int
		_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM vouchers WHERE status='pushed'`).Scan(&count)
		return count == 21
	})
	var posted, distinct int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*),count(DISTINCT client_ref) FROM mock_accounting_vouchers`).Scan(&posted, &distinct); err != nil {
		t.Fatal(err)
	}
	if posted != len(refs) || distinct != len(refs) {
		t.Fatalf("expected21 unique accounting entries; got total=%d unique=%d", posted, distinct)
	}
}
