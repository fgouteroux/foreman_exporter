package foreman

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-cleanhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newTestClient builds an HTTPClient pointed at ts with no retries and no logger.
func newTestClient(t *testing.T, ts *httptest.Server, search string) *HTTPClient {
	t.Helper()
	base, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	return NewHTTPClient(ClientConfig{
		BaseURL:     base,
		Username:    "user",
		Password:    "pass",
		Concurrency: 1,
		Search:      search,
	})
}

func TestDoWithContextSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"total":2,"page":1,"per_page":20}`))
	}))
	defer ts.Close()

	c := newTestClient(t, ts, "")
	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)

	var got HostResponse
	if err := c.DoWithContext(context.Background(), req, &got); err != nil {
		t.Fatalf("DoWithContext returned error: %v", err)
	}
	if got.Total != 2 || got.Page != 1 || got.PerPage != 20 {
		t.Fatalf("unexpected decode: %+v", got)
	}
}

func TestDoWithContextErrorBodyReturned(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer ts.Close()

	c := newTestClient(t, ts, "")
	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)

	err := c.DoWithContext(context.Background(), req, nil)
	if err == nil {
		t.Fatal("DoWithContext returned nil error on 404")
	}
	if err.Error() != `{"error":"boom"}` {
		t.Fatalf("error body = %q, want the raw response body", err.Error())
	}
}

func TestGetHostsBuildsRequest(t *testing.T) {
	var gotPath, gotSearch, gotPage, gotPerPage, gotThin, gotUser, gotPass string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		q := r.URL.Query()
		gotSearch = q.Get("search")
		gotPage = q.Get("page")
		gotPerPage = q.Get("per_page")
		gotThin = q.Get("thin")
		gotUser, gotPass, _ = r.BasicAuth()
		_, _ = w.Write([]byte(`{"total":1,"page":3,"per_page":50}`))
	}))
	defer ts.Close()

	c := newTestClient(t, ts, "os_title = CentOS")
	res, err := c.GetHosts(context.Background(), "true", 3, 50)
	if err != nil {
		t.Fatalf("GetHosts returned error: %v", err)
	}

	if gotPath != "/api/v2/hosts" {
		t.Fatalf("path = %q, want /api/v2/hosts", gotPath)
	}
	if gotSearch != "os_title = CentOS" {
		t.Fatalf("search = %q, want the configured filter", gotSearch)
	}
	if gotPage != "3" || gotPerPage != "50" {
		t.Fatalf("page/per_page = %q/%q, want 3/50", gotPage, gotPerPage)
	}
	if gotThin != "true" {
		t.Fatalf("thin = %q, want true", gotThin)
	}
	if gotUser != "user" || gotPass != "pass" {
		t.Fatalf("basic auth = %q/%q, want user/pass", gotUser, gotPass)
	}
	if res.Page != 3 || res.PerPage != 50 || res.Total != 1 {
		t.Fatalf("unexpected decoded response: %+v", res)
	}
}

func TestGetHostsOmitsThinWhenNotTrue(t *testing.T) {
	var hadThin bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadThin = r.URL.Query()["thin"]
		_, _ = w.Write([]byte(`{"total":0,"page":1,"per_page":20}`))
	}))
	defer ts.Close()

	c := newTestClient(t, ts, "")
	if _, err := c.GetHosts(context.Background(), "false", 1, 20); err != nil {
		t.Fatalf("GetHosts returned error: %v", err)
	}
	if hadThin {
		t.Fatal("thin query param present when thin != \"true\"")
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		wantOK   bool
		wantWait time.Duration
	}{
		{name: "empty", value: "", wantOK: false},
		{name: "delay seconds", value: "120", wantOK: true, wantWait: 120 * time.Second},
		{name: "zero seconds", value: "0", wantOK: true, wantWait: 0},
		{name: "negative seconds", value: "-1", wantOK: false},
		{name: "not a number", value: "soon", wantOK: false},
		{name: "http-date in the past", value: "Wed, 21 Oct 2015 07:28:00 GMT", wantOK: true, wantWait: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.value)
			if ok != tc.wantOK {
				t.Fatalf("parseRetryAfter(%q) ok = %v, want %v", tc.value, ok, tc.wantOK)
			}
			if ok && got != tc.wantWait {
				t.Fatalf("parseRetryAfter(%q) = %v, want %v", tc.value, got, tc.wantWait)
			}
		})
	}
}

func TestRetryAfterBackoffObservesMetric(t *testing.T) {
	bo := retryAfterBackoff(nil, 0)

	resp429 := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	resp429.Header.Set("Retry-After", "5")
	if got := bo(0, 0, 0, resp429); got != 5*time.Second {
		t.Fatalf("backoff(429) = %v, want 5s", got)
	}

	resp503 := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}}
	resp503.Header.Set("Retry-After", "10")
	if got := bo(0, 0, 0, resp503); got != 10*time.Second {
		t.Fatalf("backoff(503) = %v, want 10s", got)
	}

	// One histogram series per honored status; only this test observes it.
	if n := testutil.CollectAndCount(retryAfterMetric); n != 2 {
		t.Fatalf("retryAfterMetric series = %d, want 2 (429 and 503)", n)
	}
}

func TestParseRetryAfterHTTPDateFuture(t *testing.T) {
	future := time.Now().Add(1 * time.Hour).UTC().Format(http.TimeFormat)
	got, ok := parseRetryAfter(future)
	if !ok {
		t.Fatalf("parseRetryAfter(%q) ok = false, want true", future)
	}
	if got <= 0 || got > time.Hour {
		t.Fatalf("parseRetryAfter(%q) = %v, want a positive duration up to ~1h", future, got)
	}
}

func TestNewTransportKeepsPooledDefaults(t *testing.T) {
	tr := newTransport(true, 50)

	// Regression guard: this used to be a bare &http.Transport{}, which silently
	// dropped every pooled default and left MaxIdleConnsPerHost at 2.
	if tr.MaxIdleConnsPerHost != 50 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 50", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxIdleConns < 50 {
		t.Fatalf("MaxIdleConns = %d, want >= 50", tr.MaxIdleConns)
	}
	// A hard cap would serialise the two collectors when they fan out together.
	if tr.MaxConnsPerHost != 0 {
		t.Fatalf("MaxConnsPerHost = %d, want it left unlimited", tr.MaxConnsPerHost)
	}
	if tr.Proxy == nil {
		t.Fatal("Proxy is nil, want the environment proxy resolver")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = false, want true")
	}
	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify not applied")
	}
}

func TestNewTransportNeverShrinksTheDefaultPool(t *testing.T) {
	def := cleanhttp.DefaultPooledTransport()
	tr := newTransport(false, 1)
	if tr.MaxIdleConnsPerHost < def.MaxIdleConnsPerHost {
		t.Fatalf("MaxIdleConnsPerHost = %d, want at least the cleanhttp default %d",
			tr.MaxIdleConnsPerHost, def.MaxIdleConnsPerHost)
	}
}

func TestResolveIdleConns(t *testing.T) {
	tests := []struct {
		name                     string
		maxConnsPerHost, concurr int64
		want                     int
	}{
		{name: "explicit wins", maxConnsPerHost: 10, concurr: 50, want: 10},
		{name: "derived from concurrency", concurr: 50, want: 50},
		{name: "floor", concurr: 1, want: 4},
		{name: "unset", want: 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveIdleConns(tc.maxConnsPerHost, tc.concurr); got != tc.want {
				t.Fatalf("resolveIdleConns(%d, %d) = %d, want %d", tc.maxConnsPerHost, tc.concurr, got, tc.want)
			}
		})
	}
}

func TestRetryAfterBackoffCapsDelay(t *testing.T) {
	bo := retryAfterBackoff(nil, 60*time.Second)

	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}
	resp.Header.Set("Retry-After", "3600")
	if got := bo(0, 0, 0, resp); got != 60*time.Second {
		t.Fatalf("backoff(Retry-After: 3600) = %v, want the 60s cap", got)
	}

	// A delay under the cap is honored as-is.
	resp.Header.Set("Retry-After", "5")
	if got := bo(0, 0, 0, resp); got != 5*time.Second {
		t.Fatalf("backoff(Retry-After: 5) = %v, want 5s", got)
	}

	// An HTTP-date far in the future is capped too.
	resp.Header.Set("Retry-After", time.Now().Add(2*time.Hour).UTC().Format(http.TimeFormat))
	if got := bo(0, 0, 0, resp); got != 60*time.Second {
		t.Fatalf("backoff(Retry-After: +2h) = %v, want the 60s cap", got)
	}
}

// hostsPageServer serves /api/v2/hosts, echoing the requested per_page and
// handing out `subtotal` hosts spread over pages. It counts the page requests
// (the probe excluded) and can be told to fail one page.
type hostsPageServer struct {
	total     int64
	subtotal  *int64 // nil: foreman does not report the field
	failPage  string
	pageHits  atomic.Int64
	probeHits atomic.Int64
}

func (h *hostsPageServer) matching() int64 {
	if h.subtotal != nil {
		return *h.subtotal
	}
	return h.total
}

func (h *hostsPageServer) subtotalField() string {
	if h.subtotal == nil {
		return ""
	}
	return fmt.Sprintf(`"subtotal":%d,`, *h.subtotal)
}

func (h *hostsPageServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		perPage, page := q.Get("per_page"), q.Get("page")

		if perPage == "1" {
			h.probeHits.Add(1)
			fmt.Fprintf(w, `{"total":%d,%s"page":1,"per_page":1,"results":[]}`, h.total, h.subtotalField())
			return
		}
		h.pageHits.Add(1)
		if page == h.failPage {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}

		var pp, pn int64
		_, _ = fmt.Sscan(perPage, &pp)
		_, _ = fmt.Sscan(page, &pn)
		matching := h.matching()
		first := (pn - 1) * pp
		n := matching - first
		if n > pp {
			n = pp
		}
		results := make([]string, 0, max(n, 0))
		for i := int64(0); i < n; i++ {
			results = append(results, fmt.Sprintf(`{"id":%d,"name":"host-%d"}`, first+i, first+i))
		}
		fmt.Fprintf(w, `{"total":%d,%s"page":%s,"per_page":%s,"results":[%s]}`,
			h.total, h.subtotalField(), page, perPage, strings.Join(results, ","))
	}
}

func TestGetHostsFilteredPaginatesOnSubtotal(t *testing.T) {
	// total counts every host in foreman; subtotal is what the search matches.
	// Paginating on total used to request 100 pages instead of 3.
	srv := &hostsPageServer{total: 10000, subtotal: ptr(int64(250))}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	hosts, err := newTestClient(t, ts, "os_title = CentOS").GetHostsFiltered(100)
	if err != nil {
		t.Fatalf("GetHostsFiltered returned error: %v", err)
	}
	if len(hosts) != 250 {
		t.Fatalf("got %d hosts, want 250", len(hosts))
	}
	if got := srv.pageHits.Load(); got != 3 {
		t.Fatalf("page requests = %d, want 3 (ceil(250/100))", got)
	}
	// The counters are probed with per_page=1 instead of downloading a full page
	// that GetHostWithConcurrency then refetches.
	if got := srv.probeHits.Load(); got != 1 {
		t.Fatalf("probe requests = %d, want 1", got)
	}
}

func TestGetHostsFilteredFallsBackToTotal(t *testing.T) {
	// Older foreman versions may not report subtotal.
	srv := &hostsPageServer{total: 250}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	hosts, err := newTestClient(t, ts, "").GetHostsFiltered(100)
	if err != nil {
		t.Fatalf("GetHostsFiltered returned error: %v", err)
	}
	if len(hosts) != 250 {
		t.Fatalf("got %d hosts, want 250", len(hosts))
	}
	if got := srv.pageHits.Load(); got != 3 {
		t.Fatalf("page requests = %d, want 3", got)
	}
}

func TestGetHostsFilteredReportsIncompleteList(t *testing.T) {
	srv := &hostsPageServer{total: 250, subtotal: ptr(int64(250)), failPage: "2"}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	hosts, err := newTestClient(t, ts, "").GetHostsFiltered(100)
	if err == nil {
		t.Fatal("GetHostsFiltered returned nil error on a failed page")
	}
	if !strings.Contains(err.Error(), "incomplete host list") {
		t.Fatalf("error = %q, want it to name the incomplete list", err)
	}
	// The hosts from the pages that did succeed are still returned, so the
	// caller can export them instead of dropping every series.
	if len(hosts) != 150 {
		t.Fatalf("got %d hosts, want the 150 from the two successful pages", len(hosts))
	}
}

func TestGetHostsFilteredNoMatchReturnsEmpty(t *testing.T) {
	// A search matching nothing used to compute 0 pages and then block forever
	// waiting on a result channel no goroutine would ever write to.
	srv := &hostsPageServer{total: 500, subtotal: ptr(int64(0))}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	done := make(chan struct{})
	var hosts []Host
	var err error
	go func() {
		defer close(done)
		hosts, err = newTestClient(t, ts, "name = nope").GetHostsFiltered(100)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("GetHostsFiltered blocked on an empty result set")
	}
	if err != nil {
		t.Fatalf("GetHostsFiltered returned error: %v", err)
	}
	if len(hosts) != 0 {
		t.Fatalf("got %d hosts, want none", len(hosts))
	}
}

func ptr[T any](v T) *T { return &v }
