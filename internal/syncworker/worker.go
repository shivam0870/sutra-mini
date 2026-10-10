// Package syncworker delivers the durable vouchers outbox to accounting.
// Database leases distribute work; the remote client_ref uniqueness guarantee
// makes recovery safe when a worker dies after accounting accepts a request.
package syncworker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	AccountingURL string
	Concurrency   int
	HTTPTimeout   time.Duration
	LeaseDuration time.Duration
	PollInterval  time.Duration
	BaseBackoff   time.Duration
	MaxBackoff    time.Duration
	MaxAttempts   int
}

func DefaultConfig() Config {
	return Config{
		AccountingURL: "http://localhost:8081/api/vouchers",
		Concurrency:   4, HTTPTimeout: time.Second, LeaseDuration: 10 * time.Second,
		PollInterval: 250 * time.Millisecond, BaseBackoff: time.Second,
		MaxBackoff: 30 * time.Second, MaxAttempts: 5,
	}
}

type Worker struct {
	pool   *pgxpool.Pool
	cfg    Config
	client *http.Client
	logger *slog.Logger
}

// New accepts an explicitly configured worker. DefaultConfig is suitable for
// local development and serves as the starting point for environment overrides.
func New(pool *pgxpool.Pool, cfg Config, logger *slog.Logger) (*Worker, error) {
	if pool == nil {
		return nil, errors.New("worker: database pool is required")
	}
	u, err := url.Parse(cfg.AccountingURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("worker: ACCOUNTING_URL must be an absolute HTTP(S) URL")
	}
	if cfg.Concurrency < 1 || cfg.MaxAttempts < 1 || cfg.HTTPTimeout <= 0 || cfg.PollInterval <= 0 || cfg.BaseBackoff <= 0 || cfg.MaxBackoff < cfg.BaseBackoff {
		return nil, errors.New("worker: concurrency, attempts and durations must be positive; maximum backoff must cover base backoff")
	}
	if cfg.LeaseDuration <= cfg.HTTPTimeout+time.Second {
		return nil, errors.New("worker: lease duration must exceed HTTP timeout by more than one second")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{pool: pool, cfg: cfg, client: &http.Client{
		Timeout: cfg.HTTPTimeout,
		// Redirects could forward a voucher to an unintended accounting endpoint.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, logger: logger}, nil
}

type voucher struct {
	ID          int64
	Type        string
	AmountPaise int64
	ClientRef   string
	Attempts    int
	LeaseToken  string
}

// Run processes until ctx is canceled and waits for active requests to finish
// their bounded database updates before returning.
func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for slot := 0; slot < w.cfg.Concurrency; slot++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				v, err := w.claim(ctx)
				if err != nil {
					if !errors.Is(err, pgx.ErrNoRows) && ctx.Err() == nil {
						w.logger.Error("claim voucher", "error", err)
					}
					if !wait(ctx, w.cfg.PollInterval) {
						return
					}
					continue
				}
				w.deliver(ctx, v)
			}
		}()
	}
	wg.Wait()
	return nil
}

// Each processing slot claims exactly one voucher. An instance never reserves
// a batch that could sit idle while its leases expire.
func (w *Worker) claim(ctx context.Context) (voucher, error) {
	var v voucher
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return v, fmt.Errorf("generate lease token: %w", err)
	}
	v.LeaseToken = hex.EncodeToString(token)
	err := w.pool.QueryRow(ctx, `
		WITH candidate AS (
			SELECT id FROM vouchers
			WHERE status = 'pending' AND next_attempt_at <= now()
			  AND (lease_until IS NULL OR lease_until <= now())
			ORDER BY next_attempt_at, id
			FOR UPDATE SKIP LOCKED LIMIT 1
		)
		UPDATE vouchers v
		SET lease_token = $1, lease_until = now() + $2::bigint * interval '1 millisecond',
		    attempts = attempts + 1, updated_at = now()
		FROM candidate c WHERE v.id = c.id
		RETURNING v.id, v.type, v.amount_paise, v.client_ref, v.attempts`,
		v.LeaseToken, w.cfg.LeaseDuration.Milliseconds()).Scan(
		&v.ID, &v.Type, &v.AmountPaise, &v.ClientRef, &v.Attempts)
	return v, err
}

func (w *Worker) deliver(ctx context.Context, v voucher) {
	externalRef, pushErr := w.push(ctx, v)
	// If accounting accepted the push, preserve that result during graceful
	// shutdown. Hard crashes are recovered by the lease and 409 path instead.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if pushErr == nil {
		tag, err := w.pool.Exec(saveCtx, `
			UPDATE vouchers SET status = 'pushed', external_ref = $1, last_error = NULL,
			lease_token = NULL, lease_until = NULL, updated_at = now()
			WHERE id = $2 AND status = 'pending' AND lease_token = $3`, externalRef, v.ID, v.LeaseToken)
		if err != nil {
			w.logger.Error("save accounting acceptance; lease recovery will reconcile", "voucher_id", v.ID, "error", err)
		} else if tag.RowsAffected() == 1 {
			w.logger.Info("voucher pushed", "voucher_id", v.ID, "external_ref", externalRef, "attempt", v.Attempts)
		} else {
			w.logger.Warn("stale lease result discarded", "voucher_id", v.ID)
		}
		return
	}
	status := "pending"
	if v.Attempts >= w.cfg.MaxAttempts && ctx.Err() == nil {
		status = "failed"
	}
	retryDelay := backoff(w.cfg.BaseBackoff, w.cfg.MaxBackoff, v.Attempts)
	tag, err := w.pool.Exec(saveCtx, `
		UPDATE vouchers SET status = $1, last_error = $2,
		next_attempt_at = now() + $3::bigint * interval '1 millisecond',
		lease_token = NULL, lease_until = NULL, updated_at = now()
		WHERE id = $4 AND status = 'pending' AND lease_token = $5`,
		status, limitError(pushErr.Error()), retryDelay.Milliseconds(), v.ID, v.LeaseToken)
	if err != nil {
		w.logger.Error("save push failure", "voucher_id", v.ID, "error", err)
	} else if tag.RowsAffected() == 1 {
		w.logger.Warn("voucher push failed", "voucher_id", v.ID, "attempt", v.Attempts, "status", status, "error", pushErr)
	} else {
		w.logger.Warn("stale lease failure discarded", "voucher_id", v.ID)
	}
}

func (w *Worker) push(ctx context.Context, v voucher) (string, error) {
	body, err := json.Marshal(struct {
		ClientRef   string `json:"client_ref"`
		Type        string `json:"type"`
		AmountPaise int64  `json:"amount_paise"`
	}{v.ClientRef, v.Type, v.AmountPaise})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.AccountingURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("accounting request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8193))
	if err != nil {
		return "", fmt.Errorf("read accounting response: %w", err)
	}
	if len(raw) > 8192 {
		return "", errors.New("accounting response exceeds 8192 bytes")
	}
	if (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == http.StatusConflict {
		var accepted struct {
			ExternalRef string `json:"external_ref"`
		}
		if err := json.Unmarshal(raw, &accepted); err != nil || strings.TrimSpace(accepted.ExternalRef) == "" || strings.ContainsRune(accepted.ExternalRef, 0) {
			return "", fmt.Errorf("accounting HTTP %d missing valid external_ref", resp.StatusCode)
		}
		return accepted.ExternalRef, nil
	}
	return "", fmt.Errorf("accounting HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
}

func backoff(base, maximum time.Duration, attempt int) time.Duration {
	delay := base
	for i := 1; i < attempt && delay < maximum; i++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func limitError(message string) string {
	// PostgreSQL text requires valid UTF-8, including when the downstream
	// response is malformed or truncation falls inside a multi-byte rune.
	message = strings.ReplaceAll(strings.ToValidUTF8(message, "?"), "\x00", "?")
	if len(message) > 2000 {
		message = message[:2000]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
