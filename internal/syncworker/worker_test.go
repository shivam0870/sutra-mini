package syncworker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestPushReconcilesDuplicateAndPreservesClientReference(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ClientRef   string `json:"client_ref"`
			AmountPaise int64  `json:"amount_paise"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input.ClientRef != "stable-reference" || input.AmountPaise != 12500 {
			t.Errorf("incorrect accounting payload: %+v", input)
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"external_ref":"original-posting"}`)
	}))
	defer server.Close()
	w := &Worker{cfg: Config{AccountingURL: server.URL}, client: server.Client()}
	ref, err := w.push(context.Background(), voucher{ClientRef: "stable-reference", AmountPaise: 12500, Type: "jobwork_receipt"})
	if err != nil || ref != "original-posting" {
		t.Fatalf("duplicate must reconcile: ref=%q err=%v", ref, err)
	}
}

func TestPushRejectsInvalidAcceptanceAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"missing reference", http.StatusCreated, `{}`},
		{"invalid duplicate", http.StatusConflict, `{"error":"conflict"}`},
		{"blank reference", http.StatusOK, `{"external_ref":" "}`},
		{"invalid database text", http.StatusOK, `{"external_ref":"bad\u0000ref"}`},
		{"server failure", http.StatusInternalServerError, `failed`},
		{"oversized response", http.StatusCreated, strings.Repeat("x", 8193)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			w := &Worker{cfg: Config{AccountingURL: server.URL}, client: server.Client()}
			if _, err := w.push(context.Background(), voucher{}); err == nil {
				t.Fatal("invalid response must not mark a voucher pushed")
			}
		})
	}
}

func TestPushHTTPTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 40 * time.Millisecond
	w := &Worker{cfg: Config{AccountingURL: server.URL}, client: client}
	started := time.Now()
	if _, err := w.push(context.Background(), voucher{}); err == nil {
		t.Fatal("hanging response must time out")
	}
	if time.Since(started) > time.Second {
		t.Fatal("request exceeded bounded HTTP timeout")
	}
}

func TestBackoffCapsWithoutOverflow(t *testing.T) {
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 5: 10 * time.Second, 1000: 10 * time.Second} {
		if got := backoff(time.Second, 10*time.Second, attempt); got != want {
			t.Errorf("attempt %d: got %s want %s", attempt, got, want)
		}
	}
	maximum := time.Duration(1<<63 - 1)
	if got := backoff(maximum/2+1, maximum, 2); got != maximum {
		t.Fatalf("overflow guard: got %s", got)
	}
}

func TestLimitErrorPreservesPostgresText(t *testing.T) {
	for _, message := range []string{strings.Repeat("₹", 1000), strings.Repeat("a", 1999) + "₹", "invalid\xffutf8", "null\x00byte"} {
		got := limitError(message)
		if len(got) > 2000 || !utf8.ValidString(got) || strings.ContainsRune(got, 0) {
			t.Fatalf("error must fit database text: length=%d valid=%v", len(got), utf8.ValidString(got))
		}
	}
}

func TestConfigRejectsMalformedEnvironment(t *testing.T) {
	t.Setenv("WORKER_CONCURRENCY", "four")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("invalid config must return an error")
	}
	t.Setenv("WORKER_CONCURRENCY", "4")
	t.Setenv("WORKER_HTTP_TIMEOUT", "soon")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("invalid duration must return an error")
	}
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func jsonAcceptance(w http.ResponseWriter, ref string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"external_ref":%q}`, ref)
}
