package syncworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sutra/internal/testdb"
)

func insertVoucher(t *testing.T, pool *pgxpool.Pool, clientRef string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		WITH receipt AS (
			INSERT INTO jobwork_receipts(issue_id,qty,received_on,idempotency_key,request_fingerprint,response_json)
			VALUES (1,1,current_date,$1,$1,'{}'::jsonb) RETURNING id
		)
		INSERT INTO vouchers(type,source_id,amount_paise,client_ref)
		SELECT 'jobwork_receipt',id,1000,$1 FROM receipt RETURNING id`, clientRef).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func workerForTest(t *testing.T, pool *pgxpool.Pool, endpoint string) *Worker {
	t.Helper()
	cfg := DefaultConfig()
	cfg.AccountingURL = endpoint
	cfg.Concurrency = 2
	cfg.HTTPTimeout = 2 * time.Second
	cfg.LeaseDuration = 4 * time.Second
	cfg.PollInterval = 5 * time.Millisecond
	cfg.BaseBackoff = 5 * time.Millisecond
	cfg.MaxBackoff = 20 * time.Millisecond
	cfg.MaxAttempts = 3
	w, err := New(pool, cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func runWorker(t *testing.T, worker *Worker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(7 * time.Second):
			t.Error("worker did not stop after cancellation")
		}
	})
}

func awaitStatus(t *testing.T, pool *pgxpool.Pool, id int64, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(context.Background(), `SELECT status FROM vouchers WHERE id=$1`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("voucher %d status=%q, want %q", id, status, want)
}

func TestLeaseFencesStaleSuccessAndFailure(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%v", failFirst), func(t *testing.T) {
			pool := testdb.New(t)
			id := insertVoucher(t, pool, "fenced-voucher")
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestNumber := requests.Add(1)
				if failFirst && requestNumber == 1 {
					http.Error(w, "downstream failure", http.StatusServiceUnavailable)
					return
				}
				jsonAcceptance(w, "accepted-reference")
			}))
			defer server.Close()
			first := workerForTest(t, pool, server.URL)
			first.cfg.MaxAttempts = 1
			second := workerForTest(t, pool, server.URL)
			oldClaim, err := first.claim(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := second.claim(context.Background()); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("active lease must be excluded: %v", err)
			}
			if _, err := pool.Exec(context.Background(), `UPDATE vouchers SET lease_until=now()-interval '1 second' WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			newClaim, err := second.claim(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if oldClaim.LeaseToken == newClaim.LeaseToken {
				t.Fatal("reclaimed voucher must have a fresh fencing token")
			}
			first.deliver(context.Background(), oldClaim)
			var status, token string
			if err := pool.QueryRow(context.Background(), `SELECT status,lease_token FROM vouchers WHERE id=$1`, id).Scan(&status, &token); err != nil {
				t.Fatal(err)
			}
			if status != "pending" || token != newClaim.LeaseToken {
				t.Fatalf("stale worker overwrote current claim: status=%s token=%s", status, token)
			}
			second.deliver(context.Background(), newClaim)
			awaitStatus(t, pool, id, "pushed")
		})
	}
}

func TestTwoWorkerInstancesClaimEachVoucherOnce(t *testing.T) {
	pool := testdb.New(t)
	var mu sync.Mutex
	counts := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ClientRef string `json:"client_ref"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		mu.Lock()
		counts[input.ClientRef]++
		mu.Unlock()
		jsonAcceptance(w, "acct_"+input.ClientRef)
	}))
	t.Cleanup(server.Close)
	ids := make([]int64, 20)
	for i := range ids {
		ids[i] = insertVoucher(t, pool, fmt.Sprintf("two-workers-%d", i))
	}
	runWorker(t, workerForTest(t, pool, server.URL))
	runWorker(t, workerForTest(t, pool, server.URL))
	for _, id := range ids {
		awaitStatus(t, pool, id, "pushed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(counts) != len(ids) {
		t.Fatalf("accepted %d distinct references, want %d", len(counts), len(ids))
	}
	for ref, count := range counts {
		if count != 1 {
			t.Errorf("reference %s delivered %d times while leases active", ref, count)
		}
	}
}

func TestHangingVoucherDoesNotBlockOtherVouchers(t *testing.T) {
	pool := testdb.New(t)
	slowStarted := make(chan struct{}, 1)
	releaseSlow := make(chan struct{})
	fastAccepted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ClientRef string `json:"client_ref"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.ClientRef == "slow" {
			select {
			case slowStarted <- struct{}{}:
			default:
			}
			select {
			case <-releaseSlow:
			case <-r.Context().Done():
				return
			}
		} else {
			select {
			case fastAccepted <- struct{}{}:
			default:
			}
		}
		jsonAcceptance(w, "acct_"+input.ClientRef)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(releaseSlow) })
	insertVoucher(t, pool, "slow")
	runWorker(t, workerForTest(t, pool, server.URL))
	select {
	case <-slowStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("slow voucher was never claimed")
	}
	fastID := insertVoucher(t, pool, "fast")
	select {
	case <-fastAccepted:
	case <-time.After(time.Second):
		t.Fatal("a slow response blocked delivery by another processing slot")
	}
	awaitStatus(t, pool, fastID, "pushed")
}

func TestRepeatedFailuresExhaustBudgetWithReason(t *testing.T) {
	pool := testdb.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "accounting unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	id := insertVoucher(t, pool, "always-fails")
	runWorker(t, workerForTest(t, pool, server.URL))
	awaitStatus(t, pool, id, "failed")
	var attempts int
	var reason string
	if err := pool.QueryRow(context.Background(), `SELECT attempts,last_error FROM vouchers WHERE id=$1`, id).Scan(&attempts, &reason); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || !strings.Contains(reason, "503") {
		t.Fatalf("failure budget/reason: attempts=%d reason=%q", attempts, reason)
	}
	time.Sleep(50 * time.Millisecond)
	var after int
	if err := pool.QueryRow(context.Background(), `SELECT attempts FROM vouchers WHERE id=$1`, id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != attempts {
		t.Fatalf("failed voucher retried automatically: %d -> %d", attempts, after)
	}
}
