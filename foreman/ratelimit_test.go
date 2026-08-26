package foreman

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func rateLimitedClient(t *testing.T, ts *httptest.Server, reqPerSec float64, burst, retryMax int64) *HTTPClient {
	t.Helper()
	base, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	return NewHTTPClient(ClientConfig{
		BaseURL:        base,
		Concurrency:    4,
		RetryMax:       retryMax,
		RateLimit:      reqPerSec,
		RateLimitBurst: burst,
	})
}

func TestRateLimitedRoundTripperPacesRequests(t *testing.T) {
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"total":0,"page":1,"per_page":20,"results":[]}`))
	}))
	defer ts.Close()

	// 10 req/s, bucket of 1: the first goes through, the next four wait ~100ms each.
	c := rateLimitedClient(t, ts, 10, 1, 0)

	start := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := c.GetHosts(context.Background(), "true", 1, 20); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	if hits.Load() != 5 {
		t.Fatalf("server saw %d requests, want 5", hits.Load())
	}
	// Four waits of 100ms; allow slack for scheduling but require real pacing.
	if elapsed < 350*time.Millisecond {
		t.Fatalf("5 requests at 10/s took %v, want at least ~400ms", elapsed)
	}
}

func TestRateLimitedRoundTripperPacesRetries(t *testing.T) {
	// The regression this guards: a limiter placed above retryablehttp only sees
	// the first attempt of each request, so every retry escapes it — exactly the
	// traffic that piles up when foreman is already rate-limiting us.
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		// Retry-After: 0 keeps retryablehttp's own backoff out of the measurement,
		// so what remains is the limiter's pacing.
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	c := rateLimitedClient(t, ts, 10, 1, 2)

	start := time.Now()
	_, _ = c.GetHosts(context.Background(), "true", 1, 20)
	elapsed := time.Since(start)

	if got := hits.Load(); got != 3 {
		t.Fatalf("server saw %d attempts, want 3 (1 + 2 retries)", got)
	}
	// Two of the three attempts had to wait for a token.
	if elapsed < 150*time.Millisecond {
		t.Fatalf("3 paced attempts at 10/s took %v, want at least ~200ms", elapsed)
	}
}

func TestRateLimitedRoundTripperCountsWait(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total":0,"page":1,"per_page":20,"results":[]}`))
	}))
	defer ts.Close()

	c := rateLimitedClient(t, ts, 20, 1, 0)

	before := testutil.ToFloat64(rateLimitDelayedMetric)
	beforeWait := testutil.ToFloat64(rateLimitWaitMetric)
	for i := 0; i < 3; i++ {
		if _, err := c.GetHosts(context.Background(), "true", 1, 20); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}

	if delayed := testutil.ToFloat64(rateLimitDelayedMetric) - before; delayed != 2 {
		t.Fatalf("delayed requests = %v, want 2 (the first one had a token)", delayed)
	}
	if wait := testutil.ToFloat64(rateLimitWaitMetric) - beforeWait; wait <= 0 {
		t.Fatalf("wait seconds = %v, want a positive duration", wait)
	}
}

func TestRateLimitedRoundTripperHonorsContext(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total":0,"page":1,"per_page":20,"results":[]}`))
	}))
	defer ts.Close()

	// One request per minute: the second one cannot get a token in time.
	c := rateLimitedClient(t, ts, 1.0/60.0, 1, 0)
	if _, err := c.GetHosts(context.Background(), "true", 1, 20); err != nil {
		t.Fatalf("first request: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.GetHosts(ctx, "true", 1, 20); err == nil {
		t.Fatal("second request returned nil error, want the context deadline")
	}
}

func TestNewRateLimiterDisabled(t *testing.T) {
	for _, rps := range []float64{0, -1} {
		if l := newRateLimiter(rps, 0); l != nil {
			t.Fatalf("newRateLimiter(%v) = %v, want nil (disabled)", rps, l)
		}
	}
	if l := newRateLimiter(5, 0); l == nil {
		t.Fatal("newRateLimiter(5) = nil, want a limiter")
	}
}

func TestResolveBurst(t *testing.T) {
	tests := []struct {
		name  string
		rps   float64
		burst int64
		want  int
	}{
		{name: "explicit wins", rps: 16.6, burst: 5, want: 5},
		{name: "one second of traffic", rps: 16.6, want: 16},
		{name: "floor of one", rps: 0.5, want: 1},
		{name: "floor of one at exactly 1", rps: 1, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveBurst(tc.rps, tc.burst); got != tc.want {
				t.Fatalf("resolveBurst(%v, %d) = %d, want %d", tc.rps, tc.burst, got, tc.want)
			}
		})
	}
}
