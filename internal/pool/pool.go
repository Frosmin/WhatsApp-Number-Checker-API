package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"whatsapp-number-checker/internal/checker"
	"whatsapp-number-checker/internal/config"
	"whatsapp-number-checker/internal/whatsapp"
)

var (
	ErrNoWorkersAvailable = errors.New("no hay chips disponibles en el pool (ocupados, en cooldown o cuota alcanzada)")
	ErrAllQuotasExceeded  = errors.New("todos los chips alcanzaron su límite diario de consultas")
)

type GlobalStats struct {
	TargetDailyTotal  int           `json:"target_daily_total"`
	MaxDailyPerNumber int           `json:"max_daily_per_number"`
	TotalDailyChecks  int           `json:"total_daily_checks"`
	TotalChecks       int           `json:"total_checks"`
	ActiveWorkers     int           `json:"active_workers"`
	TotalWorkers      int           `json:"total_workers"`
	SuccessCount      int           `json:"success_count"`
	NegativeCount     int           `json:"negative_count"`
	ErrorCount        int           `json:"error_count"`
	RateLimitCount    int           `json:"rate_limit_count"`
	Workers           []WorkerStats `json:"workers"`
}

type CheckResult struct {
	Active   bool
	WorkerID string
}

type Pool struct {
	ctx     context.Context
	cfg     *config.Config
	logger  *slog.Logger
	mu      sync.RWMutex
	workers map[string]*Worker
}

func New(ctx context.Context, cfg *config.Config, logger *slog.Logger) *Pool {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}
	p := &Pool{
		ctx:     ctx,
		cfg:     cfg,
		logger:  logger,
		workers: make(map[string]*Worker),
	}
	go p.autoReconnectLoop()
	return p
}

func (p *Pool) Context() context.Context {
	if p.ctx != nil {
		return p.ctx
	}
	return context.Background()
}

func (p *Pool) autoReconnectLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.reconnectAll()
		}
	}
}

func (p *Pool) reconnectAll() {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, w := range p.workers {
		if w.Client != nil && w.Client.IsLoggedIn() && !w.Client.Ready() {
			p.logger.Info("Auto-reconectando chip en segundo plano", "id", w.ID, "phone", w.Client.Phone())
			_ = w.Client.Connect(p.ctx)
		}
	}
}

func (p *Pool) ReconnectWorker(id string) error {
	p.mu.RLock()
	defer p.mu.RUnlock()

	w, exists := p.workers[id]
	if !exists {
		return errors.New("chip no encontrado")
	}

	if w.Client == nil || !w.Client.IsLoggedIn() {
		return errors.New("el chip no está vinculado, requiere escanear QR")
	}

	return w.Client.Connect(p.ctx)
}

func (p *Pool) Config() *config.Config {
	return p.cfg
}

func (p *Pool) Ready() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	now := time.Now()
	for _, w := range p.workers {
		if w.IsAvailable(now, p.cfg.MaxDailyQueriesPerNumber) {
			return true
		}
	}
	return false
}

// AnyConnected returns true if at least one worker is connected to WhatsApp
func (p *Pool) AnyConnected() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, w := range p.workers {
		if w.Client.Ready() {
			return true
		}
	}
	return false
}

func (p *Pool) acquireWorker() (*Worker, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	var bestWorker *Worker
	var oldestUse time.Time
	allQuotaExceeded := true
	hasConnected := false

	for _, w := range p.workers {
		if !w.Client.Ready() {
			continue
		}
		hasConnected = true

		status := w.Status(now, p.cfg.MaxDailyQueriesPerNumber)
		if status != "quota_exceeded" {
			allQuotaExceeded = false
		}

		if w.IsAvailable(now, p.cfg.MaxDailyQueriesPerNumber) {
			last := w.LastUsed()
			if bestWorker == nil || last.Before(oldestUse) {
				bestWorker = w
				oldestUse = last
			}
		}
	}

	if !hasConnected {
		return nil, checker.ErrUnavailable
	}

	if bestWorker == nil {
		if allQuotaExceeded {
			return nil, ErrAllQuotasExceeded
		}
		return nil, ErrNoWorkersAvailable
	}

	bestWorker.SetBusy(true)
	return bestWorker, nil
}

func (p *Pool) Check(ctx context.Context, phone string) (bool, error) {
	res, err := p.CheckWithDetails(ctx, phone)
	return res.Active, err
}

func (p *Pool) CheckWithDetails(ctx context.Context, phone string) (CheckResult, error) {
	worker, err := p.acquireWorker()
	if err != nil {
		return CheckResult{}, err
	}

	registered, err := worker.Check(ctx, phone)
	worker.RecordResult(registered, err, p.cfg.RequestDelay)

	if err != nil {
		return CheckResult{WorkerID: worker.ID}, err
	}

	return CheckResult{
		Active:   registered,
		WorkerID: worker.ID,
	}, nil
}

func (p *Pool) Stats() GlobalStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	now := time.Now()
	stats := GlobalStats{
		TargetDailyTotal:  p.cfg.TargetDailyTotal,
		MaxDailyPerNumber: p.cfg.MaxDailyQueriesPerNumber,
		TotalWorkers:      len(p.workers),
		Workers:           make([]WorkerStats, 0, len(p.workers)),
	}

	for _, w := range p.workers {
		ws := w.Stats(now, p.cfg.MaxDailyQueriesPerNumber)
		stats.Workers = append(stats.Workers, ws)
		stats.TotalDailyChecks += ws.DailyChecks
		stats.TotalChecks += ws.TotalChecks
		stats.SuccessCount += ws.SuccessCount
		stats.NegativeCount += ws.NegativeCount
		stats.ErrorCount += ws.ErrorCount
		stats.RateLimitCount += ws.RateLimitCount

		if ws.Status == "online" || ws.Status == "busy" {
			stats.ActiveWorkers++
		}
	}

	return stats
}

func (p *Pool) AddWorker(id, alias, path string, client WhatsAppClient) *Worker {
	p.mu.Lock()
	defer p.mu.Unlock()

	if existing, exists := p.workers[id]; exists {
		_ = existing.Close()
	}

	w := NewWorker(id, alias, path, client)
	p.workers[id] = w
	return w
}

func (p *Pool) RemoveSession(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	worker, exists := p.workers[id]
	if !exists {
		return errors.New("sesión no encontrada")
	}

	_ = worker.Close()
	delete(p.workers, id)

	if worker.Path != "" {
		_ = os.Remove(worker.Path)
		_ = os.Remove(worker.Path + "-wal")
		_ = os.Remove(worker.Path + "-shm")
	}

	return nil
}

func (p *Pool) GetWorker(id string) (*Worker, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	w, ok := p.workers[id]
	return w, ok
}

func (p *Pool) LoadSessions(ctx context.Context) error {
	dir := p.cfg.SessionsDir
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("crear carpeta de sesiones: %w", err)
	}

	// Migración automática de data/session.db heredado
	legacyPath := "data/session.db"
	if fi, err := os.Stat(legacyPath); err == nil && !fi.IsDir() {
		migratedPath := filepath.Join(dir, "default.db")
		if _, err := os.Stat(migratedPath); os.IsNotExist(err) {
			_ = os.Rename(legacyPath, migratedPath)
			p.logger.Info("Sesión anterior migrada a pool", "from", legacyPath, "to", migratedPath)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("leer carpeta de sesiones: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}

		sessionID := strings.TrimSuffix(entry.Name(), ".db")
		sessionPath := filepath.Join(dir, entry.Name())

		client, err := whatsapp.Open(p.Context(), sessionPath, p.logger.With("session", sessionID))
		if err != nil {
			p.logger.Error("No se pudo abrir sesión", "id", sessionID, "error", err)
			continue
		}

		worker := p.AddWorker(sessionID, sessionID, sessionPath, client)

		go func(w *Worker, cl *whatsapp.Client) {
			if err := cl.Start(p.Context(), io.Discard); err != nil && p.ctx.Err() == nil {
				p.logger.Warn("Error iniciando sesión", "id", w.ID, "error", err)
			}
		}(worker, client)

		p.logger.Info("Sesión cargada en pool", "id", sessionID, "path", sessionPath)
	}

	return nil
}

func (p *Pool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for id, w := range p.workers {
		_ = w.Close()
		delete(p.workers, id)
	}
}
