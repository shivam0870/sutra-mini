// Package mockaccounting implements a deliberately unreliable accounting API.
// Its accepted ledger is durable so restarting either process cannot erase
// the client_ref idempotency guarantee.
package mockaccounting

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Mode         string
	HangDuration time.Duration
}

func DefaultConfig() Config { return Config{Mode: "random", HangDuration: 5 * time.Second} }

func ConfigFromEnv() Config {
	cfg := DefaultConfig()
	if mode := os.Getenv("MOCK_MODE"); mode != "" {
		cfg.Mode = mode
	}
	return cfg
}

type Server struct {
	pool   *pgxpool.Pool
	cfg    Config
	logger *slog.Logger
}

func New(pool *pgxpool.Pool, cfg Config, logger *slog.Logger) (*Server, error) {
	if pool == nil {
		return nil, errors.New("mock: database pool is required")
	}
	switch cfg.Mode {
	case "random", "success", "fail", "hang":
	default:
		return nil, errors.New("mock: MOCK_MODE must be random, success, fail or hang")
	}
	if cfg.HangDuration <= 0 {
		return nil, errors.New("mock: hang duration must be positive")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{pool: pool, cfg: cfg, logger: logger}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if err := s.pool.Ping(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "mode": s.cfg.Mode})
	})
	mux.HandleFunc("POST /api/vouchers", s.createVoucher)
	return mux
}

type request struct {
	ClientRef   string `json:"client_ref"`
	Type        string `json:"type"`
	AmountPaise int64  `json:"amount_paise"`
}

func (s *Server) createVoucher(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var input request
	if err := dec.Decode(&input); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid voucher JSON"})
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF || strings.TrimSpace(input.ClientRef) == "" || len(input.ClientRef) > 200 || strings.TrimSpace(input.Type) == "" || len(input.Type) > 100 || input.AmountPaise <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "one voucher with client_ref, type and positive integer amount_paise is required"})
		return
	}
	// Already accepted references reconcile immediately, even in failure mode.
	// This lookup is an optimization; the PRIMARY KEY is the actual guarantee.
	var externalRef string
	err := s.pool.QueryRow(r.Context(), `SELECT external_ref FROM mock_accounting_vouchers WHERE client_ref=$1`, input.ClientRef).Scan(&externalRef)
	if err == nil {
		s.duplicate(w, input.ClientRef, externalRef)
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		s.databaseError(w, err)
		return
	}
	mode := s.cfg.Mode
	if mode == "random" {
		mode = outcome(mathrand.IntN(100))
	}
	if mode == "fail" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "simulated accounting failure"})
		return
	}
	refBytes := make([]byte, 16)
	if _, err := rand.Read(refBytes); err != nil {
		s.databaseError(w, err)
		return
	}
	externalRef = "acct_" + hex.EncodeToString(refBytes)
	err = s.pool.QueryRow(r.Context(), `
		INSERT INTO mock_accounting_vouchers(client_ref,external_ref,amount_paise,type)
		VALUES ($1,$2,$3,$4) ON CONFLICT (client_ref) DO NOTHING RETURNING external_ref`,
		input.ClientRef, externalRef, input.AmountPaise, input.Type).Scan(&externalRef)
	if errors.Is(err, pgx.ErrNoRows) {
		// Concurrent callers may both pass the initial lookup. Only one inserts;
		// the other returns precisely that winner's reference.
		err = s.pool.QueryRow(r.Context(), `SELECT external_ref FROM mock_accounting_vouchers WHERE client_ref=$1`, input.ClientRef).Scan(&externalRef)
		if err == nil {
			s.duplicate(w, input.ClientRef, externalRef)
			return
		}
	}
	if err != nil {
		s.databaseError(w, err)
		return
	}
	s.logger.Info("accounting accepted voucher", "client_ref", input.ClientRef, "external_ref", externalRef, "mode", mode)
	if mode == "hang" {
		// Persist BEFORE the slow response: killing the caller here represents
		// an uncertain outcome. A later request sees the durable duplicate.
		time.Sleep(s.cfg.HangDuration)
	}
	writeJSON(w, http.StatusCreated, map[string]string{"external_ref": externalRef})
}

func outcome(draw int) string {
	if draw < 20 {
		return "fail"
	}
	if draw < 30 {
		return "hang"
	}
	return "success"
}

func (s *Server) duplicate(w http.ResponseWriter, clientRef, externalRef string) {
	s.logger.Info("accounting duplicate reconciled", "client_ref", clientRef, "external_ref", externalRef)
	writeJSON(w, http.StatusConflict, map[string]string{"error": "duplicate client_ref", "external_ref": externalRef})
}

func (s *Server) databaseError(w http.ResponseWriter, err error) {
	s.logger.Error("mock accounting persistence", "error", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "accounting persistence failed"})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
