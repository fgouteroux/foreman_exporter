package foreman

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"
)

var (
	rateLimitWaitMetric = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "foreman_exporter_client_rate_limit_wait_seconds_total",
		Help: "Cumulative time foreman client requests spent waiting on the client-side rate limiter.",
	})

	rateLimitDelayedMetric = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "foreman_exporter_client_rate_limit_delayed_requests_total",
		Help: "A counter of foreman client requests that were held back by the client-side rate limiter.",
	})

	rateLimitMetric = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "foreman_exporter_client_rate_limit_requests_per_second",
		Help: "The client-side rate limit currently applied to foreman requests, 0 when disabled.",
	})
)

// rateLimitedRoundTripper paces outgoing requests through a token bucket.
//
// It sits at the RoundTripper level rather than in DoWithContext on purpose:
// retryablehttp loops internally over the underlying client, so a limiter
// placed above it would only see the first attempt of each request and let
// every retry through unpaced — precisely the traffic that piles up when
// foreman is already rate-limiting us.
//
// It also wraps the promhttp instrumentation rather than sitting under it, so
// the time spent waiting for a token is not counted in
// foreman_exporter_client_request_duration_seconds (which should measure
// foreman) nor in the in-flight gauge (which should measure concurrency).
type rateLimitedRoundTripper struct {
	next    http.RoundTripper
	limiter *rate.Limiter
}

func (rt *rateLimitedRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	// Fast path: a token is available, no bookkeeping needed.
	if !rt.limiter.Allow() {
		start := time.Now()
		if err := rt.limiter.Wait(r.Context()); err != nil {
			return nil, err
		}
		rateLimitWaitMetric.Add(time.Since(start).Seconds())
		rateLimitDelayedMetric.Inc()
	}
	return rt.next.RoundTrip(r)
}

// newRateLimiter builds the token bucket from the configured requests per
// second. It returns nil when rate limiting is disabled, so the RoundTripper
// chain stays untouched in that case.
func newRateLimiter(requestsPerSecond float64, burst int64) *rate.Limiter {
	rateLimitMetric.Set(requestsPerSecond)
	if requestsPerSecond <= 0 {
		return nil
	}
	return rate.NewLimiter(rate.Limit(requestsPerSecond), resolveBurst(requestsPerSecond, burst))
}

// resolveBurst defaults the bucket depth to roughly one second of traffic:
// enough to keep the workers fed, small enough that a burst cannot meaningfully
// overshoot a server-side fixed window.
func resolveBurst(requestsPerSecond float64, burst int64) int {
	if burst > 0 {
		return int(burst)
	}
	if b := int(requestsPerSecond); b > 1 {
		return b
	}
	return 1
}
