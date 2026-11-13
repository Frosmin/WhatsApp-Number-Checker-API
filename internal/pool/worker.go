package pool

import (
	"context"
	"errors"
	"sync"
	"time"

	"whatsapp-number-checker/internal/checker"
)

type WorkerStats struct {
	ID             string    `json:"id"`
	Alias          string    `json:"alias"`
	Phone          string    `json:"phone"`
	Status         string    `json:"status"` // "online", "busy", "cooldown", "quota_exceeded", "disconnected", "logged_out"
	DailyChecks    int       `json:"daily_checks"`
	MaxDaily       int       `json:"max_daily"`
	TotalChecks    int       `json:"total_checks"`
	SuccessCount   int       `json:"success_count"`
	NegativeCount  int       `json:"negative_count"`
	ErrorCount     int       `json:"error_count"`
	RateLimitCount int       `json:"rate_limit_count"`
	LastUsed       time.Time `json:"last_used"`
	CooldownSec    int64     `json:"cooldown_sec"`
}

type WhatsAppClient interface {
	checker.Checker
	Phone() string
	IsLoggedIn() bool
	Connect(ctx context.Context) error
	Close() error
	SetOnStatusChange(fn func(string))
}

type Worker struct {
	ID             string
	Alias          string
	Path           string
	Client         WhatsAppClient
	mu             sync.Mutex
	busy           bool
	cooldownUntil  time.Time
	lastUsed       time.Time
	dailyDate      string
	dailyChecks    int
	totalChecks    int
	successCount   int
	negativeCount  int
	errorCount     int
	rateLimitCount int
	createdAt      time.Time
}

func NewWorker(id, alias, path string, client WhatsAppClient) *Worker {
	if alias == "" {
		alias = id
	}
	now := time.Now()
	w := &Worker{
		ID:        id,
		Alias:     alias,
		Path:      path,
		Client:    client,
		dailyDate: now.Format("2006-01-02"),
		createdAt: now,
	}

	client.SetOnStatusChange(func(status string) {
		// Callback on connection events
	})

	return w
}

func (w *Worker) checkRolloverLocked(today string) {
	if w.dailyDate != today {
		w.dailyDate = today
		w.dailyChecks = 0
	}
}

func (w *Worker) Status(now time.Time, maxDaily int) string {
	w.mu.Lock()
	defer w.mu.Unlock()

	today := now.Format("2006-01-02")
	w.checkRolloverLocked(today)

	if !w.Client.Ready() {
		if !w.Client.IsLoggedIn() {
			return "logged_out"
		}
		return "disconnected"
	}

	if w.busy {
		return "busy"
	}

	if now.Before(w.cooldownUntil) {
		return "cooldown"
	}

	if maxDaily > 0 && w.dailyChecks >= maxDaily {
		return "quota_exceeded"
	}

	return "online"
}

func (w *Worker) IsAvailable(now time.Time, maxDaily int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	today := now.Format("2006-01-02")
	w.checkRolloverLocked(today)

	if !w.Client.Ready() {
		return false
	}
	if w.busy {
		return false
	}
	if now.Before(w.cooldownUntil) {
		return false
	}
	if maxDaily > 0 && w.dailyChecks >= maxDaily {
		return false
	}
	return true
}

func (w *Worker) LastUsed() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastUsed
}

func (w *Worker) SetBusy(busy bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.busy = busy
	if busy {
		w.lastUsed = time.Now()
	}
}

func (w *Worker) SetCooldown(duration time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cooldownUntil = time.Now().Add(duration)
	w.rateLimitCount++
}

func (w *Worker) RecordResult(registered bool, err error, delay time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.busy = false
	now := time.Now()
	w.lastUsed = now
	today := now.Format("2006-01-02")
	w.checkRolloverLocked(today)

	w.dailyChecks++
	w.totalChecks++

	if delay > 0 {
		w.cooldownUntil = now.Add(delay)
	}

	if err != nil {
		var rateErr *checker.RateLimitError
		if errors.As(err, &rateErr) {
			w.rateLimitCount++
			wait := max(time.Minute, rateErr.RetryAfter)
			w.cooldownUntil = now.Add(wait)
		} else {
			w.errorCount++
		}
		return
	}

	if registered {
		w.successCount++
	} else {
		w.negativeCount++
	}
}

func (w *Worker) Stats(now time.Time, maxDaily int) WorkerStats {
	w.mu.Lock()
	defer w.mu.Unlock()

	today := now.Format("2006-01-02")
	w.checkRolloverLocked(today)

	var cooldownSec int64
	if now.Before(w.cooldownUntil) {
		cooldownSec = int64(w.cooldownUntil.Sub(now).Seconds())
	}

	status := "online"
	if !w.Client.Ready() {
		if !w.Client.IsLoggedIn() {
			status = "logged_out"
		} else {
			status = "disconnected"
		}
	} else if w.busy {
		status = "busy"
	} else if cooldownSec > 0 {
		status = "cooldown"
	} else if maxDaily > 0 && w.dailyChecks >= maxDaily {
		status = "quota_exceeded"
	}

	phone := w.Client.Phone()

	return WorkerStats{
		ID:             w.ID,
		Alias:          w.Alias,
		Phone:          phone,
		Status:         status,
		DailyChecks:    w.dailyChecks,
		MaxDaily:       maxDaily,
		TotalChecks:    w.totalChecks,
		SuccessCount:   w.successCount,
		NegativeCount:  w.negativeCount,
		ErrorCount:     w.errorCount,
		RateLimitCount: w.rateLimitCount,
		LastUsed:       w.lastUsed,
		CooldownSec:    cooldownSec,
	}
}

func (w *Worker) Check(ctx context.Context, phone string) (bool, error) {
	return w.Client.Check(ctx, phone)
}

func (w *Worker) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.busy = false
	return w.Client.Close()
}
