// Package api implements the transactional job-work receipt API.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const pricePerUnitPaise int64 = 2500

type server struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) http.Handler {
	s := &server{pool: pool}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /issues", s.issues)
	mux.HandleFunc("GET /issues/{id}", s.issue)
	mux.HandleFunc("POST /jobwork/receipts", s.receive)
	mux.HandleFunc("GET /vouchers", s.vouchers)
	mux.HandleFunc("POST /vouchers/{id}/retry", s.retry)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

type Issue struct {
	ID          int64  `json:"id"`
	PartyID     int64  `json:"party_id"`
	PartyName   string `json:"party_name"`
	SKUID       int64  `json:"sku_id"`
	SKUCode     string `json:"sku_code"`
	SKUName     string `json:"sku_name"`
	ImageURL    string `json:"image_url"`
	IssuedOn    string `json:"issued_on"`
	Issued      int64  `json:"issued"`
	Received    int64  `json:"received"`
	Outstanding int64  `json:"outstanding"`
}

type Receipt struct {
	ID             int64     `json:"id"`
	IssueID        int64     `json:"issue_id"`
	Qty            int64     `json:"qty"`
	ReceivedOn     string    `json:"received_on"`
	IdempotencyKey string    `json:"idempotency_key"`
	CreatedAt      time.Time `json:"created_at"`
}

type Voucher struct {
	ID            int64     `json:"id"`
	Type          string    `json:"type"`
	SourceID      int64     `json:"source_id"`
	AmountPaise   int64     `json:"amount_paise"`
	Status        string    `json:"status"`
	Attempts      int       `json:"attempts"`
	LastError     *string   `json:"last_error"`
	ClientRef     string    `json:"client_ref"`
	ExternalRef   *string   `json:"external_ref"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
}

type rowScanner interface{ Scan(...any) error }

const issueSelect = `SELECT i.id, p.id, p.name, s.id, s.code, s.name, s.image_url,
    i.issued_on::text, i.qty, i.received_qty, i.qty - i.received_qty
    FROM jobwork_issues i JOIN parties p ON p.id = i.party_id JOIN skus s ON s.id = i.sku_id`

const voucherColumns = `id, type, source_id, amount_paise, status, attempts,
    last_error, client_ref, external_ref, created_at, updated_at, next_attempt_at`

func scanIssue(row rowScanner) (Issue, error) {
	var v Issue
	err := row.Scan(&v.ID, &v.PartyID, &v.PartyName, &v.SKUID, &v.SKUCode, &v.SKUName,
		&v.ImageURL, &v.IssuedOn, &v.Issued, &v.Received, &v.Outstanding)
	return v, err
}

func scanVoucher(row rowScanner) (Voucher, error) {
	var v Voucher
	err := row.Scan(&v.ID, &v.Type, &v.SourceID, &v.AmountPaise, &v.Status, &v.Attempts,
		&v.LastError, &v.ClientRef, &v.ExternalRef, &v.CreatedAt, &v.UpdatedAt, &v.NextAttemptAt)
	return v, err
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.pool.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) issues(w http.ResponseWriter, r *http.Request) {
	rows, err := s.pool.Query(r.Context(), issueSelect+` ORDER BY i.id`)
	if err != nil {
		internalError(w, err)
		return
	}
	defer rows.Close()
	items := []Issue{}
	for rows.Next() {
		item, err := scanIssue(rows)
		if err != nil {
			internalError(w, err)
			return
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": items})
}

func (s *server) issue(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	item, err := scanIssue(s.pool.QueryRow(r.Context(), issueSelect+` WHERE i.id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "issue not found")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

type receiptInput struct {
	IssueID    int64  `json:"issue_id"`
	Qty        int64  `json:"qty"`
	ReceivedOn string `json:"received_on"`
}

func (s *server) receive(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if strings.TrimSpace(key) == "" || len(key) > 200 {
		writeError(w, http.StatusBadRequest, "Idempotency-Key must contain 1 to 200 bytes")
		return
	}
	var input receiptInput
	if !decodeJSON(w, r, &input) {
		return
	}
	date, err := time.Parse("2006-01-02", input.ReceivedOn)
	if input.IssueID <= 0 || input.Qty <= 0 || input.Qty > math.MaxInt64/pricePerUnitPaise || err != nil || date.Year() < 1 {
		writeError(w, http.StatusBadRequest, "issue_id and qty must be positive integers; received_on must be YYYY-MM-DD; amount must fit int64")
		return
	}
	canonical, _ := json.Marshal(input)
	digest := sha256.Sum256(canonical)
	fingerprint := hex.EncodeToString(digest[:])
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	defer tx.Rollback(context.Background())
	// Serialize a key before checking it. The UNIQUE constraint remains the
	// durable guarantee; hash collisions only cause harmless extra waiting.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		internalError(w, err)
		return
	}
	var existingFingerprint, saved string
	err = tx.QueryRow(ctx, `SELECT request_fingerprint, response_json::text FROM jobwork_receipts WHERE idempotency_key=$1`, key).Scan(&existingFingerprint, &saved)
	if err == nil {
		if existingFingerprint != fingerprint {
			writeError(w, http.StatusConflict, "Idempotency-Key was already used with a different request")
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		writeRawJSON(w, http.StatusCreated, saved)
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		internalError(w, err)
		return
	}
	receipt := Receipt{IssueID: input.IssueID, Qty: input.Qty, ReceivedOn: input.ReceivedOn, IdempotencyKey: key}
	err = tx.QueryRow(ctx, `INSERT INTO jobwork_receipts
        (issue_id,qty,received_on,idempotency_key,request_fingerprint,response_json)
        VALUES ($1,$2,$3::date,$4,$5,'{}'::jsonb) RETURNING id,created_at`,
		input.IssueID, input.Qty, date, key, fingerprint).Scan(&receipt.ID, &receipt.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "receipt_capacity" {
			writeError(w, http.StatusConflict, "receipt exceeds the issue's outstanding quantity")
		} else if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			writeError(w, http.StatusNotFound, "issue not found")
		} else {
			internalError(w, err)
		}
		return
	}
	voucher, err := scanVoucher(tx.QueryRow(ctx, `INSERT INTO vouchers(type,source_id,amount_paise,client_ref)
        VALUES ('jobwork_receipt',$1,$2,$3) RETURNING `+voucherColumns,
		receipt.ID, input.Qty*pricePerUnitPaise, "jobwork-receipt-"+strconv.FormatInt(receipt.ID, 10)))
	if err != nil {
		internalError(w, err)
		return
	}
	response, err := json.Marshal(struct {
		Receipt Receipt `json:"receipt"`
		Voucher Voucher `json:"voucher"`
	}{receipt, voucher})
	if err != nil {
		internalError(w, err)
		return
	}
	// Return the JSON representation stored by PostgreSQL on BOTH paths, so
	// replay remains byte-for-byte identical even after the worker updates it.
	if err := tx.QueryRow(ctx, `UPDATE jobwork_receipts SET response_json=$1::jsonb WHERE id=$2 RETURNING response_json::text`, string(response), receipt.ID).Scan(&saved); err != nil {
		internalError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		internalError(w, err)
		return
	}
	writeRawJSON(w, http.StatusCreated, saved)
}

func (s *server) vouchers(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status != "" && status != "pending" && status != "pushed" && status != "failed" {
		writeError(w, http.StatusBadRequest, "status must be pending, pushed, or failed")
		return
	}
	rows, err := s.pool.Query(r.Context(), `SELECT `+voucherColumns+` FROM vouchers WHERE ($1::text='' OR status=$1) ORDER BY created_at DESC,id DESC`, status)
	if err != nil {
		internalError(w, err)
		return
	}
	defer rows.Close()
	items := []Voucher{}
	for rows.Next() {
		v, err := scanVoucher(rows)
		if err != nil {
			internalError(w, err)
			return
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vouchers": items})
}

func (s *server) retry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	v, err := scanVoucher(s.pool.QueryRow(r.Context(), `UPDATE vouchers SET status='pending',
        attempts=0,last_error=NULL,next_attempt_at=now(),lease_token=NULL,lease_until=NULL,updated_at=now()
        WHERE id=$1 AND status='failed' RETURNING `+voucherColumns, id))
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := s.pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM vouchers WHERE id=$1)`, id).Scan(&exists); err != nil {
			internalError(w, err)
			return
		}
		if !exists {
			writeError(w, http.StatusNotFound, "voucher not found")
		} else {
			writeError(w, http.StatusConflict, "only failed vouchers can be retried")
		}
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"voucher": v})
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "id must be a positive integer")
		return 0, false
	}
	return id, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dest any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dest); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON request body")
		return false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "request body must contain one JSON object")
		return false
	}
	return true
}

func internalError(w http.ResponseWriter, err error) {
	slog.Error("API database operation", "error", err)
	writeError(w, http.StatusInternalServerError, "database operation failed")
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("write JSON response", "error", err)
	}
}

func writeRawJSON(w http.ResponseWriter, status int, value string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, value+"\n"); err != nil {
		slog.Error("write saved response", "error", err)
	}
}
