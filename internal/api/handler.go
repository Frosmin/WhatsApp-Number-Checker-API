package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"rsc.io/qr"

	"whatsapp-number-checker/internal/checker"
	"whatsapp-number-checker/internal/pool"
	"whatsapp-number-checker/internal/whatsapp"
	"whatsapp-number-checker/web"
)

type Handler struct {
	checker checker.Checker
	pool    *pool.Pool
	timeout time.Duration
	now     func() time.Time
	mu      sync.Mutex
	busy    bool
	next    time.Time
}

func New(c checker.Checker) *Handler {
	h := &Handler{
		checker: c,
		timeout: 10 * time.Second,
		now:     time.Now,
	}
	if p, ok := c.(*pool.Pool); ok {
		h.pool = p
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := h.serve(w, r)
	// Never log query strings, phone numbers, or QR contents in access logs
	slog.Info("petición HTTP", "method", r.Method, "path", r.URL.Path, "status", status, "duration_ms", time.Since(start).Milliseconds())
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) int {
	// Web Dashboard
	if r.URL.Path == "/" || r.URL.Path == "/admin" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			return writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(web.IndexHTML)
		return http.StatusOK
	}

	// API Stats for Dashboard
	if r.URL.Path == "/api/stats" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if h.pool == nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_daily_checks": 0,
				"active_workers":     0,
				"total_workers":      0,
				"workers":            []any{},
			})
			return http.StatusOK
		}
		_ = json.NewEncoder(w).Encode(h.pool.Stats())
		return http.StatusOK
	}

	// Pairing SSE Stream
	if r.URL.Path == "/api/sessions/pair/stream" {
		return h.handlePairStream(w, r)
	}

	// Remove session
	if r.URL.Path == "/api/sessions" && r.Method == http.MethodDelete {
		return h.handleDeleteSession(w, r)
	}

	// Reconnect session
	if r.URL.Path == "/api/sessions/reconnect" {
		return h.handleReconnectSession(w, r)
	}

	// Consult endpoint
	if r.URL.Path == "/check" {
		return h.handleCheck(w, r)
	}

	w.Header().Set("Content-Type", "application/json")
	return writeError(w, http.StatusNotFound, "not_found")
}

func (h *Handler) handleCheck(w http.ResponseWriter, r *http.Request) int {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

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

	// If using Pool, concurrency and per-worker rate limiting is handled by the pool
	if h.pool != nil {
		ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
		defer cancel()

		res, err := h.pool.CheckWithDetails(ctx, phone)
		if err != nil {
			return h.mapCheckError(w, err, ctx)
		}

		_ = json.NewEncoder(w).Encode(struct {
			Number string `json:"number"`
			Active bool   `json:"active"`
			Worker string `json:"worker,omitempty"`
		}{
			Number: phone,
			Active: res.Active,
			Worker: res.WorkerID,
		})
		return http.StatusOK
	}

	// Legacy single checker rate limit
	if wait := h.acquire(); wait > 0 {
		return writeRateLimit(w, wait)
	}
	defer h.release()

	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	registered, err := h.checker.Check(ctx, phone)
	if err != nil {
		var limited *checker.RateLimitError
		if errors.As(err, &limited) {
			wait := max(time.Minute, limited.RetryAfter)
			h.mu.Lock()
			h.next = h.now().Add(wait)
			h.mu.Unlock()
			return writeRateLimit(w, wait)
		}
		return h.mapCheckError(w, err, ctx)
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

func (h *Handler) mapCheckError(w http.ResponseWriter, err error, ctx context.Context) int {
	var limited *checker.RateLimitError
	if errors.As(err, &limited) {
		wait := max(time.Minute, limited.RetryAfter)
		return writeRateLimit(w, wait)
	}
	if errors.Is(err, pool.ErrAllQuotasExceeded) {
		return writeError(w, http.StatusTooManyRequests, "all_quotas_exceeded")
	}
	if errors.Is(err, pool.ErrNoWorkersAvailable) {
		return writeRateLimit(w, 2*time.Second)
	}
	if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return writeError(w, http.StatusGatewayTimeout, "timeout")
	}
	if errors.Is(err, checker.ErrUnavailable) {
		return writeError(w, http.StatusServiceUnavailable, "whatsapp_unavailable")
	}
	return writeError(w, http.StatusBadGateway, "upstream_error")
}

func (h *Handler) handlePairStream(w http.ResponseWriter, r *http.Request) int {
	sessionID := r.URL.Query().Get("id")
	if sessionID == "" {
		w.Header().Set("Content-Type", "application/json")
		return writeError(w, http.StatusBadRequest, "missing_session_id")
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		return writeError(w, http.StatusInternalServerError, "streaming_unsupported")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	appCtx := context.Background()
	if h.pool != nil {
		appCtx = h.pool.Context()
	}

	// If already in pool and connected:
	if h.pool != nil {
		if worker, exists := h.pool.GetWorker(sessionID); exists && worker.Client.Ready() {
			data, _ := json.Marshal(map[string]string{
				"id":    sessionID,
				"phone": worker.Client.Phone(),
			})
			fmt.Fprintf(w, "event: connected\ndata: %s\n\n", data)
			flusher.Flush()
			return http.StatusOK
		}
	}

	sessionsDir := "data/sessions"
	if h.pool != nil && h.pool.Config() != nil && h.pool.Config().SessionsDir != "" {
		sessionsDir = h.pool.Config().SessionsDir
	}
	sessionPath := filepath.Join(sessionsDir, sessionID+".db")

	client, err := whatsapp.Open(appCtx, sessionPath, slog.Default().With("pairing", sessionID))
	if err != nil {
		data, _ := json.Marshal(map[string]string{"error": err.Error()})
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
		flusher.Flush()
		return http.StatusOK
	}

	if client.IsLoggedIn() {
		_ = client.Start(appCtx, io.Discard)
		for i := 0; i < 20; i++ {
			if client.Ready() {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if client.Ready() {
			if h.pool != nil {
				h.pool.AddWorker(sessionID, sessionID, sessionPath, client)
			}
			data, _ := json.Marshal(map[string]string{
				"id":    sessionID,
				"phone": client.Phone(),
			})
			fmt.Fprintf(w, "event: connected\ndata: %s\n\n", data)
			flusher.Flush()
			return http.StatusOK
		}
	}

	qrChan, err := client.GetQRChannel(appCtx)
	if err != nil {
		_ = client.Close()
		data, _ := json.Marshal(map[string]string{"error": err.Error()})
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
		flusher.Flush()
		return http.StatusOK
	}

	go func() {
		_ = client.Start(appCtx, io.Discard)
	}()

	connectedChan := make(chan struct{})
	client.SetOnStatusChange(func(st string) {
		if st == "connected" {
			select {
			case <-connectedChan:
			default:
				close(connectedChan)
			}
		}
	})

	for {
		select {
		case <-r.Context().Done():
			// If client was never paired, close it
			if !client.IsLoggedIn() {
				_ = client.Close()
			}
			return http.StatusOK
		case <-connectedChan:
			if h.pool != nil {
				h.pool.AddWorker(sessionID, sessionID, sessionPath, client)
			}
			data, _ := json.Marshal(map[string]string{
				"id":    sessionID,
				"phone": client.Phone(),
			})
			fmt.Fprintf(w, "event: connected\ndata: %s\n\n", data)
			flusher.Flush()
			return http.StatusOK
		case item, ok := <-qrChan:
			if !ok {
				return http.StatusOK
			}
			switch item.Event {
			case whatsmeow.QRChannelEventCode:
				qrCode, err := qr.Encode(item.Code, qr.M)
				if err == nil {
					pngBase64 := base64.StdEncoding.EncodeToString(qrCode.PNG())
					data, _ := json.Marshal(map[string]string{
						"image": "data:image/png;base64," + pngBase64,
						"code":  item.Code,
					})
					fmt.Fprintf(w, "event: qr\ndata: %s\n\n", data)
					flusher.Flush()
				}
			case whatsmeow.QRChannelSuccess.Event:
				data, _ := json.Marshal(map[string]string{"message": "Vinculado, conectando a WhatsApp..."})
				fmt.Fprintf(w, "event: status\ndata: %s\n\n", data)
				flusher.Flush()
			}
		}
	}
}

func (h *Handler) handleReconnectSession(w http.ResponseWriter, r *http.Request) int {
	w.Header().Set("Content-Type", "application/json")
	id := r.URL.Query().Get("id")
	if id == "" {
		return writeError(w, http.StatusBadRequest, "missing_session_id")
	}

	if h.pool == nil {
		return writeError(w, http.StatusNotFound, "pool_unavailable")
	}

	if err := h.pool.ReconnectWorker(id); err != nil {
		return writeError(w, http.StatusInternalServerError, err.Error())
	}

	_ = json.NewEncoder(w).Encode(map[string]string{"status": "reconnecting"})
	return http.StatusOK
}

func (h *Handler) handleDeleteSession(w http.ResponseWriter, r *http.Request) int {
	w.Header().Set("Content-Type", "application/json")
	id := r.URL.Query().Get("id")
	if id == "" {
		return writeError(w, http.StatusBadRequest, "missing_session_id")
	}

	if h.pool == nil {
		return writeError(w, http.StatusNotFound, "session_not_found")
	}

	if err := h.pool.RemoveSession(id); err != nil {
		return writeError(w, http.StatusNotFound, "session_not_found")
	}

	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
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
