package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"whatsapp-number-checker/internal/checker"
)

type fakeChecker struct {
	ready bool
	check func(context.Context, string) (bool, error)
}

func (f *fakeChecker) Ready() bool { return f.ready }
func (f *fakeChecker) Check(ctx context.Context, phone string) (bool, error) {
	return f.check(ctx, phone)
}

func request(h *Handler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", target, nil))
	return w
}

func TestBooleanAndNormalization(t *testing.T) {
	for _, tc := range []struct {
		query, phone, body string
		registered         bool
	}{
		{"59170000000", "+59170000000", "true\n", true},
		{"%2B59170000000", "+59170000000", "false\n", false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			h := New(&fakeChecker{true, func(_ context.Context, phone string) (bool, error) {
				if phone != tc.phone {
					t.Errorf("phone = %q, want %q", phone, tc.phone)
				}
				return tc.registered, nil
			}})
			w := request(h, "/check?number="+tc.query)
			if w.Code != 200 || w.Body.String() != tc.body {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if w.Header().Get("Content-Type") != "application/json" {
				t.Fatal("expected JSON")
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("result could be cached")
			}
		})
	}
}

func TestInvalidNumbersNeverReachWhatsApp(t *testing.T) {
	for _, query := range []string{"", "number=", "number=0123", "number=591x", "number=591-123", "number=+591123", "number=%20591123", "number=%2B", "number=1234567890123456", "number=１２３", "number=123&number=456", "number=123&number=123", "number=%ZZ", "number=123;other=4"} {
		t.Run(query, func(t *testing.T) {
			h := New(&fakeChecker{true, func(context.Context, string) (bool, error) {
				t.Fatal("invalid input reached provider")
				return false, nil
			}})
			w := request(h, "/check?"+query)
			if w.Code != 400 || w.Body.String() != "{\"error\":\"invalid_number\"}\n" {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}

func TestErrorsAreNeverFalse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready bool
		err   error
		code  int
		body  string
	}{
		{"not linked", false, nil, 503, "whatsapp_unavailable"},
		{"disconnected during lookup", true, checker.ErrUnavailable, 503, "whatsapp_unavailable"},
		{"upstream", true, errors.New("network failure"), 502, "upstream_error"},
		{"ambiguous", true, checker.ErrInvalidResponse, 502, "upstream_error"},
		{"deadline", true, context.DeadlineExceeded, 504, "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(&fakeChecker{tc.ready, func(context.Context, string) (bool, error) {
				if !tc.ready {
					t.Fatal("unready connection queried")
				}
				return false, tc.err
			}})
			w := request(h, "/check?number=59170000000")
			if w.Code != tc.code || w.Body.String() != "{\"error\":\""+tc.body+"\"}\n" {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}

func TestTimeoutCancelsProviderAndReleasesSlot(t *testing.T) {
	var calls atomic.Int32
	h := New(&fakeChecker{true, func(ctx context.Context, _ string) (bool, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			return false, ctx.Err()
		}
		return true, nil
	}})
	h.timeout = 10 * time.Millisecond
	w := request(h, "/check?number=59170000000")
	if w.Code != 504 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	h.now = func() time.Time { return time.Now().Add(2 * time.Second) }
	if w := request(h, "/check?number=59170000000"); w.Code != 200 {
		t.Fatalf("slot not released: %d", w.Code)
	}
}

func TestRateLimitAndNoResultCache(t *testing.T) {
	var calls int
	now := time.Now()
	h := New(&fakeChecker{true, func(context.Context, string) (bool, error) { calls++; return calls == 1, nil }})
	h.now = func() time.Time { return now }
	if w := request(h, "/check?number=59170000000"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	w := request(h, "/check?number=59170000000")
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	now = now.Add(time.Second)
	w = request(h, "/check?number=59170000000")
	if w.Code != 200 || w.Body.String() != "false\n" {
		t.Fatalf("cached or blocked: %d %s", w.Code, w.Body)
	}
}

func TestConcurrentRequestIsRejectedWithoutQueue(t *testing.T) {
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan *httptest.ResponseRecorder, 1)
	h := New(&fakeChecker{true, func(context.Context, string) (bool, error) { close(entered); <-release; return true, nil }})
	go func() { finished <- request(h, "/check?number=59170000000") }()
	<-entered
	w := request(h, "/check?number=59170000001")
	close(release)
	first := <-finished
	if w.Code != 429 || first.Code != 200 {
		t.Fatalf("first=%d second=%d", first.Code, w.Code)
	}
}

func TestUpstreamCooldown(t *testing.T) {
	for _, tc := range []struct {
		wait    time.Duration
		seconds string
		block   time.Duration
	}{
		{0, "60", 60 * time.Second}, {125 * time.Second, "125", 125 * time.Second},
	} {
		t.Run(tc.seconds, func(t *testing.T) {
			now := time.Now()
			calls := 0
			h := New(&fakeChecker{true, func(context.Context, string) (bool, error) {
				calls++
				if calls == 1 {
					return false, &checker.RateLimitError{RetryAfter: tc.wait}
				}
				return true, nil
			}})
			h.now = func() time.Time { return now }
			w := request(h, "/check?number=59170000000")
			if w.Code != 429 || w.Header().Get("Retry-After") != tc.seconds {
				t.Fatalf("%d %v", w.Code, w.Header())
			}
			now = now.Add(tc.block - time.Millisecond)
			if w := request(h, "/check?number=59170000001"); w.Code != 429 {
				t.Fatal("cooldown did not cover other numbers")
			}
			if calls != 1 {
				t.Fatal("retried during cooldown")
			}
			now = now.Add(time.Millisecond)
			if w := request(h, "/check?number=59170000000"); w.Code != 200 {
				t.Fatal("cooldown never ended")
			}
		})
	}
}

func TestRouteAndMethod(t *testing.T) {
	h := New(&fakeChecker{ready: true})
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{"POST", "/check", 405}, {"HEAD", "/check", 405}, {"GET", "/other", 404},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.code || !strings.Contains(w.Body.String(), "error") {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
}
