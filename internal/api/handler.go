package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"whatsapp-number-checker/internal/checker"
)

type Handler struct {
	checker checker.Checker
	timeout time.Duration
	now     func() time.Time
	mu      sync.Mutex
	busy    bool
	next    time.Time
}

func New(c checker.Checker) *Handler {
	return &Handler{checker: c, timeout: 10 * time.Second, now: time.Now}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := h.serve(w, r)
	// Never log the query string, phone number, credentials, or QR contents.
	slog.Info("petición HTTP", "method", r.Method, "status", status, "duration_ms", time.Since(start).Milliseconds())
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) int {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path != "/check" {
		return writeError(w, http.StatusNotFound, "not_found")
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		return writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
	}
	phone, ok := parseNumber(r.URL.RawQuery)
	if !ok {
		return writeError(w, http.StatusBadRequest, "invalid_number")
	}
	if !h.checker.Ready() {
		return writeError(w, http.StatusServiceUnavailable, "whatsapp_unavailable")
	}
	if wait := h.acquire(); wait > 0 {
		return writeRateLimit(w, wait)
	}
	defer h.release()
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	registered, err := h.checker.Check(ctx, phone)
	var limited *checker.RateLimitError
	if errors.As(err, &limited) {
		wait := max(time.Minute, limited.RetryAfter)
		h.mu.Lock()
		h.next = h.now().Add(wait)
		h.mu.Unlock()
		return writeRateLimit(w, wait)
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return writeError(w, http.StatusGatewayTimeout, "timeout")
	}
	if errors.Is(err, checker.ErrUnavailable) {
		return writeError(w, http.StatusServiceUnavailable, "whatsapp_unavailable")
	}
	if err != nil {
		return writeError(w, http.StatusBadGateway, "upstream_error")
	}
	_ = json.NewEncoder(w).Encode(struct {
		Number string `json:"number"`
		Active bool   `json:"active"`
	}{
		Number: phone,
		Active: registered,
	})
	return http.StatusOK

}

func parseNumber(rawQuery string) (string, bool) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil || len(query["number"]) != 1 {
		return "", false
	}
	number := strings.TrimPrefix(query["number"][0], "+")
	if len(number) == 0 || len(number) > 15 || number[0] == '0' {
		return "", false
	}
	for _, digit := range number {
		if digit < '0' || digit > '9' {
			return "", false
		}
	}
	return "+" + number, true
}

func (h *Handler) acquire() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	wait := h.next.Sub(now)
	if h.busy || wait > 0 {
		return max(time.Second, wait)
	}
	h.busy = true
	h.next = now.Add(time.Second)
	return 0
}

func (h *Handler) release() {
	h.mu.Lock()
	h.busy = false
	h.mu.Unlock()
}

func writeRateLimit(w http.ResponseWriter, wait time.Duration) int {
	w.Header().Set("Retry-After", strconv.FormatInt(int64(math.Ceil(wait.Seconds())), 10))
	return writeError(w, http.StatusTooManyRequests, "rate_limited")
}

func writeError(w http.ResponseWriter, status int, code string) int {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{code})
	return status
}
