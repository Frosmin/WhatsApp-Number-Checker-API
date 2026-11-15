package pool

import (
	"context"
	"sync"
	"testing"
	"time"

	"whatsapp-number-checker/internal/checker"
	"whatsapp-number-checker/internal/config"
)

type mockClient struct {
	mu        sync.Mutex
	ready     bool
	phone     string
	checkFunc func(context.Context, string) (bool, error)
}

func (m *mockClient) Ready() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready
}

func (m *mockClient) Check(ctx context.Context, phone string) (bool, error) {
	if m.checkFunc != nil {
		return m.checkFunc(ctx, phone)
	}
	return true, nil
}

func (m *mockClient) Phone() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.phone
}

func (m *mockClient) IsLoggedIn() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready
}

func (m *mockClient) Connect(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ready = true
	return nil
}

func (m *mockClient) Close() error {
	return nil
}

func (m *mockClient) SetOnStatusChange(fn func(string)) {}

func TestPoolLongestIdleSelection(t *testing.T) {
	cfg := &config.Config{
		MaxDailyQueriesPerNumber: 100,
		TargetDailyTotal:         1000,
		RequestDelay:             0,
	}
	p := New(context.Background(), cfg, nil)

	c1 := &mockClient{ready: true, phone: "111"}
	c2 := &mockClient{ready: true, phone: "222"}

	w1 := p.AddWorker("chip1", "Chip 1", "", c1)
	w2 := p.AddWorker("chip2", "Chip 2", "", c2)

	// Set w1 as used 5 seconds ago, w2 as used 10 seconds ago
	now := time.Now()
	w1.lastUsed = now.Add(-5 * time.Second)
	w2.lastUsed = now.Add(-10 * time.Second)

	// Should select w2 because it has been idle longer (10s > 5s)
	res, err := p.CheckWithDetails(context.Background(), "59170000000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.WorkerID != "chip2" {
		t.Fatalf("expected worker chip2, got %s", res.WorkerID)
	}
}

func TestPoolMaxDailyQuota(t *testing.T) {
	cfg := &config.Config{
		MaxDailyQueriesPerNumber: 2,
		TargetDailyTotal:         10,
		RequestDelay:             0,
	}
	p := New(context.Background(), cfg, nil)

	c1 := &mockClient{ready: true, phone: "111"}
	p.AddWorker("chip1", "Chip 1", "", c1)

	// Query 1
	if _, err := p.Check(context.Background(), "123"); err != nil {
		t.Fatalf("query 1 failed: %v", err)
	}
	// Query 2
	if _, err := p.Check(context.Background(), "123"); err != nil {
		t.Fatalf("query 2 failed: %v", err)
	}

	// Query 3 should fail with ErrAllQuotasExceeded
	_, err := p.Check(context.Background(), "123")
	if err != ErrAllQuotasExceeded {
		t.Fatalf("expected ErrAllQuotasExceeded, got: %v", err)
	}

	stats := p.Stats()
	if stats.Workers[0].Status != "quota_exceeded" {
		t.Fatalf("expected status quota_exceeded, got: %s", stats.Workers[0].Status)
	}
}

func TestPoolCooldownIsolation(t *testing.T) {
	cfg := &config.Config{
		MaxDailyQueriesPerNumber: 100,
		TargetDailyTotal:         1000,
		RequestDelay:             0,
	}
	p := New(context.Background(), cfg, nil)

	// chip1 will fail with 429
	c1 := &mockClient{
		ready: true,
		phone: "111",
		checkFunc: func(ctx context.Context, s string) (bool, error) {
			return false, &checker.RateLimitError{RetryAfter: 2 * time.Minute}
		},
	}
	// chip2 will succeed
	c2 := &mockClient{
		ready: true,
		phone: "222",
		checkFunc: func(ctx context.Context, s string) (bool, error) {
			return true, nil
		},
	}

	w1 := p.AddWorker("chip1", "Chip 1", "", c1)
	w2 := p.AddWorker("chip2", "Chip 2", "", c2)

	// Set w1 as oldest
	w1.lastUsed = time.Now().Add(-10 * time.Minute)
	w2.lastUsed = time.Now().Add(-1 * time.Minute)

	// First query uses chip1, which fails with 429 and enters cooldown
	_, err := p.Check(context.Background(), "123")
	if err == nil {
		t.Fatalf("expected rate limit error")
	}

	// Second query should automatically pick chip2 because chip1 is in cooldown!
	res, err := p.CheckWithDetails(context.Background(), "123")
	if err != nil {
		t.Fatalf("expected query on chip2 to succeed, got: %v", err)
	}
	if res.WorkerID != "chip2" {
		t.Fatalf("expected chip2, got %s", res.WorkerID)
	}

	// Verify stats
	stats := p.Stats()
	for _, w := range stats.Workers {
		if w.ID == "chip1" {
			if w.Status != "cooldown" {
				t.Fatalf("expected chip1 to be in cooldown, got %s", w.Status)
			}
			if w.RateLimitCount != 1 {
				t.Fatalf("expected rate limit count 1, got %d", w.RateLimitCount)
			}
		}
	}
}
